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

// Sync pushes locally produced data to every configured peer. The push itself
// is also this server's availability probe, so each round yields one
// reachability record per peer.
type Sync struct {
	Store      *store.Store
	Client     *peer.Client
	ServerID   int64
	ServerUID  string
	ServerName string
	Location   string
	PublicURL  string
	Alerts     bool
	Interval   time.Duration
	Log        *slog.Logger

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

	start := model.At(time.Now().Add(-backfillWindow))
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

	timestamp := model.Now()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(target model.Server) {
			defer wg.Done()
			w.push(ctx, target, payload, timestamp)
		}(p)
	}
	wg.Wait()

	w.lastMetric, w.lastAvail, w.lastNotice = marks.metric, marks.avail, marks.notice
	w.Log.Debug("sync round complete", "metrics", len(payload.Metrics), "peers", len(peers))
	return nil
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

func (w *Sync) push(ctx context.Context, target model.Server, payload model.SyncPayload, timestamp model.Time) {
	ok, latencyMs, status := w.Client.PushSync(ctx, target.URL, payload)

	if err := w.Store.InsertAvailability(w.ServerID, []store.Observation{{
		ToServerID:  target.ID,
		Timestamp:   timestamp,
		IsAvailable: ok,
		LatencyMs:   &latencyMs,
		HTTPStatus:  status,
	}}); err != nil {
		w.Log.Error("record availability", "peer", target.Name, "err", err)
		return
	}

	if ok {
		if err := w.Store.TouchLastSeen(target.ID, timestamp); err != nil {
			w.Log.Error("touch peer last seen", "peer", target.Name, "err", err)
		}
	}

	w.Log.Debug("sync push", "peer", target.Name, "ok", ok, "latencyMs", latencyMs)
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
		Metrics:      metrics,
		Availability: availability,
		Notices:      notices,
	}, marks, nil
}
