package worker

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/peer"
	"github.com/m0n5ter/m0nit0r/internal/series"
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

	// How far each peer has been brought up to date in the series streams,
	// keyed by its row id. Unlike the three above this cannot be one mark for
	// everybody: a bucket is published once and not re-offered, so a peer that
	// joined late or was unreachable has to be owed exactly what it missed.
	peerMarks map[int64]seriesMarks
}

// Run synchronises until ctx is cancelled.
func (w *Sync) Run(ctx context.Context) {
	w.Log.Info("peer sync started", "interval", w.Interval)

	w.started = time.Now()
	w.peerMarks = map[int64]seriesMarks{}
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

	// The original form of the payload is filled only while somebody in the
	// mesh still reads it. Once every peer speaks the series form it is dead
	// weight in every round, and the two queries behind it stop being run.
	legacy := false
	for _, p := range peers {
		if p.Protocol < model.ProtocolVersion {
			legacy = true
			break
		}
	}

	payload, marks, err := w.buildPayload(legacy)
	if err != nil {
		return err
	}

	// The series streams are cut per peer, because how far each of them has
	// been brought up to date is a fact about that peer and not about this
	// node. A neighbour met a minute ago is owed the history; one synced with
	// all along is owed the last thirty seconds.
	sent := make([]seriesMarks, len(peers))
	payloads := make([]model.SyncPayload, len(peers))
	for i, p := range peers {
		payloads[i], sent[i], err = w.forPeer(payload, p, timestamp)
		if err != nil {
			return err
		}
	}

	replies := make([]*model.SyncReply, len(peers))
	delivered := make([]bool, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, target model.Server) {
			defer wg.Done()
			replies[i], delivered[i] = w.push(ctx, target, payloads[i], timestamp)
		}(i, p)
	}
	wg.Wait()

	w.lastMetric, w.lastAvail, w.lastNotice = marks.metric, marks.avail, marks.notice

	// Only what arrived counts as offered. The three original streams advance
	// regardless, as they always have - they are a window that the next round
	// re-offers anyway - but a bucket is published once, so a peer that was
	// unreachable this round has to be owed it again next round.
	for i := range peers {
		if delivered[i] {
			w.peerMarks[peers[i].ID] = sent[i]
		}
	}

	w.Log.Debug("sync round complete", "metrics", len(payload.Metrics), "peers", len(peers))

	w.learnPeers(replies)
	return nil
}

// maxSeriesRows caps how much of the series store one push may carry.
//
// A peer meeting this node for the first time is owed everything the ladder
// holds, which is a year of daily rows and a fortnight of finer ones for every
// node in the mesh. Sent in one request that would be megabytes; sent a
// capped slice at a time it is a handful of rounds, and the high-water mark
// coming back from each query is what makes the next round resume exactly
// where this one stopped.
const maxSeriesRows = 4000

// seriesMarks is how far one peer has been brought up to date: the newest
// reading it has been offered, and the newest bucket at each rung.
type seriesMarks struct {
	sample model.Time
	bucket map[series.Level]int64
	known  bool
}

// forPeer cuts the payload down to what this particular peer is owed.
//
// Readings are taken up to the start of the round and no further. The round's
// own probes are stamped with that instant but not written until the pushes
// come back, and the metric ticker runs on the same beat as the round, so a
// reading from a few milliseconds into it is there on every round or on none.
// Letting it through carried the mark past the probes each time, and they were
// never sent: the node's metrics arrived everywhere while its row in every
// other node's matrix stayed empty. Whatever is held back is next round's.
func (w *Sync) forPeer(base model.SyncPayload, target model.Server, round model.Time) (model.SyncPayload, seriesMarks, error) {
	payload := base
	marks := w.peerMarks[target.ID]

	// A peer too old to understand the series form gets the original one and
	// nothing else. It is the only reason the original is still filled.
	if target.Protocol < model.ProtocolVersion {
		return payload, marks, nil
	}
	payload.Metrics, payload.Availability = nil, nil

	if !marks.known {
		marks = w.openingMarks()
	}
	// A map is a reference, so advancing the copy would advance what is stored
	// whether or not the push ever arrives. This round works on its own.
	marks.bucket = maps.Clone(marks.bucket)
	if marks.bucket == nil {
		marks.bucket = map[series.Level]int64{}
	}

	samples, high, err := w.Store.OwnSamplesSince(w.ServerID, marks.sample, round, maxSeriesRows)
	if err != nil {
		return payload, marks, err
	}
	payload.Samples, marks.sample = samples, high

	// Coarsest first, so that a node joining a mesh shows a year of history
	// within a round or two and fills the detail in behind it.
	budget := maxSeriesRows
	nowMs := model.Now().DB()
	for i := len(series.Ladder) - 1; i >= 0 && budget > 0; i-- {
		rung := series.Ladder[i]
		rows, high, err := w.Store.OwnRollups(w.ServerID, rung.Level,
			marks.bucket[rung.Level], series.SealedThrough(nowMs, rung.Level), budget)
		if err != nil {
			return payload, marks, err
		}
		payload.Rollups = append(payload.Rollups, rows...)
		marks.bucket[rung.Level] = high
		budget -= len(rows)
	}

	return payload, marks, nil
}

// openingMarks is where a peer never synced with before is started from: far
// enough back that it receives the history this node holds rather than only
// what happens from now on.
func (w *Sync) openingMarks() seriesMarks {
	marks := seriesMarks{
		sample: model.At(time.Now().Add(-backfillWindow)),
		bucket: map[series.Level]int64{},
		known:  true,
	}
	for _, rung := range series.Ladder {
		oldest, ok, err := w.Store.OldestOwnBucket(w.ServerID, rung.Level)
		if err != nil {
			w.Log.Error("oldest own bucket", "level", rung.Level, "err", err)
			continue
		}
		if ok {
			// Exclusive, so the oldest bucket itself is included.
			marks.bucket[rung.Level] = oldest - 1
		}
	}
	return marks
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
// A server already known is left alone, apart from one carrying no address:
// either such a nameless row, or a node just added back into the mesh, whose
// address only its neighbours know. A node the mesh has removed is not among
// them - its tombstone is what a neighbour's peer list cannot overrule.
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
func (w *Sync) push(ctx context.Context, target model.Server, payload model.SyncPayload, timestamp model.Time) (*model.SyncReply, bool) {
	res := w.Client.PushSync(ctx, target.URL, payload)

	// The peer says this node was removed from its mesh. That is not an
	// outage - there is nothing wrong with either end - so it is not recorded
	// as one; the peer is simply dropped, and with it the rest of the mesh as
	// each of them answers the same way. Nothing is announced from here: the
	// decision was somebody else's, and leaving it unrecorded is what lets the
	// peer's first push back in if this node is ever added again.
	if res.Status != nil && *res.Status == http.StatusGone {
		w.Log.Info("peer says this node was removed from its mesh", "peer", target.Name)
		if err := w.Store.ForgetPeer(target.ID); err != nil {
			w.Log.Error("forget peer", "peer", target.Name, "err", err)
		}
		return nil, false
	}

	if err := w.Store.InsertAvailability(w.ServerID, []store.Observation{{
		ToServerID:  target.ID,
		Timestamp:   timestamp,
		IsAvailable: res.OK,
		LatencyMs:   &res.LatencyMs,
		HTTPStatus:  res.Status,
	}}); err != nil {
		w.Log.Error("record availability", "peer", target.Name, "err", err)
		return res.Reply, res.OK
	}

	if res.OK {
		if err := w.Store.TouchLastSeen(target.ID, timestamp); err != nil {
			w.Log.Error("touch peer last seen", "peer", target.Name, "err", err)
		}
	}

	w.Log.Debug("sync push", "peer", target.Name, "ok", res.OK, "latencyMs", res.LatencyMs)
	return res.Reply, res.OK
}

// buildPayload gathers everything produced locally since the last round, and
// reports the newest timestamp in each stream so the watermarks can advance.
func (w *Sync) buildPayload(legacy bool) (model.SyncPayload, watermarks, error) {
	marks := watermarks{w.lastMetric, w.lastAvail, w.lastNotice}

	// Both come out of the series store, not the tables the first build kept
	// them in. Which form of the payload a peer understands is a question
	// about the wire; where the readings are held is not its business, and
	// keeping the two separate is what lets those tables be dropped while
	// nodes that predate them are still being spoken to.
	var (
		metrics      []model.Metric
		availability []model.Availability
		err          error
	)
	if legacy {
		if metrics, err = w.Store.MetricsRaw(w.ServerID, w.lastMetric); err != nil {
			return model.SyncPayload{}, marks, err
		}
		if availability, err = w.Store.OwnReachabilitySince(w.ServerID, w.lastAvail); err != nil {
			return model.SyncPayload{}, marks, err
		}
	}

	// What this node has already announced about its peers. Carried so that
	// the other alerting nodes can see the message was sent and not send it
	// again - and, when this node stops sending them, so that they can see it
	// stopped and take over.
	notices, err := w.Store.OwnNoticesSince(w.ServerID, w.lastNotice)
	if err != nil {
		return model.SyncPayload{}, marks, err
	}

	// Which nodes have been taken out of the mesh, and which have been added
	// back. Unlike the three streams above this is not a window: it is every
	// decision this node holds, repeated in full each round, whoever made it.
	// It is one row per node ever removed, so it costs nothing to resend, and
	// resending is what carries a removal to a node that was down when it was
	// made or that the removing node does not push to at all.
	removals, err := w.Store.Membership()
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
		Protocol:     model.ProtocolVersion,
		ServerName:   w.ServerName,
		Location:     w.Location,
		SelfURL:      w.PublicURL,
		Alerts:       w.Alerts,
		PushOnly:     w.PushOnly,
		Metrics:      metrics,
		Availability: availability,
		Notices:      notices,
		Removals:     removals,
	}, marks, nil
}
