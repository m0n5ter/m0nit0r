package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/peer"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// backfillWindow is how much history the first sync round after start-up
// offers a peer, covering a short restart without replaying everything.
const backfillWindow = 10 * time.Minute

// minPushGrace is the floor on how long a push-only peer may go unheard before
// it counts as down. See Sync.pushGrace.
const minPushGrace = 30 * time.Second

// Sync pushes locally produced data to every configured peer. The push itself
// is also this server's availability probe, so each round yields one
// reachability record per peer.
//
// Peers that are push-only cannot be probed that way, having no address. For
// them the round records instead whether their own pushes are still arriving,
// which gives them the same one record per round and so the same matrix cell,
// online state and alerts as any other peer.
//
// Every push is answered with the peers the other end syncs with, and this node
// adopts the ones it does not know, so the mesh closes over itself from any one
// introduction. A push-only node has no other way to learn it at all, since
// nobody can reach it to introduce the rest.
type Sync struct {
	Store      *store.Store
	Client     *peer.Client
	ServerID   int64
	ServerUID  string
	ServerName string
	Location   string
	PublicURL  string
	Alerts     bool
	PushOnly   bool
	Interval   time.Duration
	Log        *slog.Logger

	// When the worker started. A push-only peer's last push predates it by
	// however long this node was down, which says nothing about the peer.
	started time.Time

	// Watermarks of what has already been offered to peers. Advancing them by
	// the newest row actually sent — rather than by wall clock — means samples
	// written while a round is in flight are picked up next time instead of
	// being skipped.
	lastMetric model.Time
	lastAvail  model.Time
	lastNotice model.Time
}

// Run synchronises until ctx is cancelled.
func (w *Sync) Run(ctx context.Context) {
	w.Log.Info("peer sync started", "interval", w.Interval)

	w.started = time.Now()
	start := model.At(w.started.Add(-backfillWindow))
	w.lastMetric, w.lastAvail, w.lastNotice = start, start, start

	// Let the first metrics land before the opening round, so a fresh peer is
	// not handed an empty payload.
	delay := min(w.Interval, 30*time.Second)
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		if err := w.round(ctx); err != nil {
			w.Log.Error("sync to peers", "err", err)
		}

		select {
		case <-ctx.Done():
			w.Log.Info("peer sync stopped")
			return
		case <-ticker.C:
		}
	}
}

func (w *Sync) round(ctx context.Context) error {
	timestamp := model.Now()

	if err := w.watchPushOnly(timestamp); err != nil {
		w.Log.Error("check push-only peers", "err", err)
	}

	peers, err := w.Store.ListPeers()
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return nil
	}

	payload, marks, err := w.buildPayload()
	if err != nil {
		return err
	}

	replies := make([]*model.SyncReply, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, target model.Server) {
			defer wg.Done()
			replies[i] = w.push(ctx, target, payload, timestamp)
		}(i, p)
	}
	wg.Wait()

	w.lastMetric, w.lastAvail, w.lastNotice = marks.metric, marks.avail, marks.notice
	w.Log.Debug("sync round complete", "metrics", len(payload.Metrics), "peers", len(peers))

	w.learnPeers(replies)
	return nil
}

// pushGrace is how long a push-only peer may go unheard before it counts as
// down: three of this node's rounds, so that one push lost to a slow link or a
// restart does not register, and never less than minPushGrace, so that a node
// configured to sync faster than its push-only peers does not mark every gap
// between their pushes as an outage.
func (w *Sync) pushGrace() time.Duration {
	return max(3*w.Interval, minPushGrace)
}

// watchPushOnly records, for each push-only peer, whether it has pushed here
// recently. There is no round trip to time, so the observation carries no
// latency; the dashboard shows such an edge as received rather than measured.
func (w *Sync) watchPushOnly(timestamp model.Time) error {
	grace := w.pushGrace()
	// Right after start-up every peer's last push is as old as this node's own
	// downtime. Recording that as the peer being down would put a false
	// outage in the matrix after every restart.
	if timestamp.Sub(w.started) < grace {
		return nil
	}

	pushing, err := w.Store.ListPushOnly()
	if err != nil {
		return err
	}
	if len(pushing) == 0 {
		return nil
	}

	observations := make([]store.Observation, 0, len(pushing))
	for _, p := range pushing {
		observations = append(observations, store.Observation{
			ToServerID:  p.ID,
			Timestamp:   timestamp,
			IsAvailable: timestamp.Sub(p.LastSeen.Time) <= grace,
		})
	}
	return w.Store.InsertAvailability(w.ServerID, observations)
}

// learnPeers registers the peers named in the replies to this round's pushes,
// so that a node pointed at any one member of a mesh ends up pushing to all of
// them - and so that the members end up pushing back, each of them learning
// this node from the replies to their own pushes. Without it a node joined
// through a single neighbour stays a mesh of two, while the ids of everybody
// else trickle in through that neighbour's reachability reports as nameless
// rows nothing is ever collected for.
//
// A server already known is left alone, apart from one that is only such a
// nameless row; that is what keeps a peer the operator removed here from being
// brought back by a neighbour.
func (w *Sync) learnPeers(replies []*model.SyncReply) {
	for _, reply := range replies {
		if reply == nil {
			continue
		}
		for _, p := range reply.Peers {
			url := peer.NormalizeURL(p.URL)
			if p.ServerID == "" || p.ServerID == w.ServerUID || url == "" {
				continue
			}
			added, err := w.Store.AddPeerIfUnknown(p.ServerID, p.ServerName, p.Location, url, p.Alerts)
			if err != nil {
				w.Log.Error("learn peer", "peer", p.ServerID, "err", err)
				continue
			}
			if added {
				w.Log.Info("learned peer from sync reply", "peer", p.ServerName, "url", url)
			}
		}
	}
}

// watermarks is how far each of the three streams has been offered to peers.
type watermarks struct{ metric, avail, notice model.Time }

// advance moves a watermark to t when t is newer, which is how the newest row
// actually sent is found without sorting what was read.
func advance(mark *model.Time, t model.Time) {
	if t.After(mark.Time) {
		*mark = t
	}
}

// push sends one round's payload to one peer, records how it went, and hands
// back whatever the peer replied with.
func (w *Sync) push(ctx context.Context, target model.Server, payload model.SyncPayload, timestamp model.Time) *model.SyncReply {
	res := w.Client.PushSync(ctx, target.URL, payload)

	if err := w.Store.InsertAvailability(w.ServerID, []store.Observation{{
		ToServerID:  target.ID,
		Timestamp:   timestamp,
		IsAvailable: res.OK,
		LatencyMs:   &res.LatencyMs,
		HTTPStatus:  res.Status,
	}}); err != nil {
		w.Log.Error("record availability", "peer", target.Name, "err", err)
		return res.Reply
	}

	if res.OK {
		if err := w.Store.TouchLastSeen(target.ID, timestamp); err != nil {
			w.Log.Error("touch peer last seen", "peer", target.Name, "err", err)
		}
	}

	w.Log.Debug("sync push", "peer", target.Name, "ok", res.OK, "latencyMs", res.LatencyMs)
	return res.Reply
}

// buildPayload gathers everything produced locally since the last round, and
// reports the newest timestamp in each stream so the watermarks can advance.
func (w *Sync) buildPayload() (model.SyncPayload, watermarks, error) {
	marks := watermarks{w.lastMetric, w.lastAvail, w.lastNotice}

	metrics, err := w.Store.MetricsSince(w.ServerID, w.lastMetric)
	if err != nil {
		return model.SyncPayload{}, marks, err
	}

	availability, err := w.Store.OwnAvailabilitySince(w.ServerID, w.lastAvail)
	if err != nil {
		return model.SyncPayload{}, marks, err
	}

	// What this node has already announced about its peers. Carried so that
	// the other alerting nodes can see the message was sent and not send it
	// again - and, when this node stops sending them, so that they can see it
	// stopped and take over.
	notices, err := w.Store.OwnNoticesSince(w.ServerID, w.lastNotice)
	if err != nil {
		return model.SyncPayload{}, marks, err
	}

	for _, m := range metrics {
		advance(&marks.metric, m.Timestamp)
	}
	for _, a := range availability {
		advance(&marks.avail, a.Timestamp)
	}
	for _, n := range notices {
		advance(&marks.notice, n.Timestamp)
	}

	return model.SyncPayload{
		ServerID:     w.ServerUID,
		ServerName:   w.ServerName,
		Location:     w.Location,
		SelfURL:      w.PublicURL,
		Alerts:       w.Alerts,
		PushOnly:     w.PushOnly,
		Metrics:      metrics,
		Availability: availability,
		Notices:      notices,
	}, marks, nil
}
