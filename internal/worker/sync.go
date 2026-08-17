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
	ServerID   string
	ServerName string
	Location   string
	PublicURL  string
	Interval   time.Duration
	Log        *slog.Logger

	// Watermarks of what has already been offered to peers. Advancing them by
	// the newest row actually sent — rather than by wall clock — means samples
	// written while a round is in flight are picked up next time instead of
	// being skipped.
	lastMetric model.Time
	lastAvail  model.Time
}

// Run synchronises until ctx is cancelled.
func (w *Sync) Run(ctx context.Context) {
	w.Log.Info("peer sync started", "interval", w.Interval)

	start := model.At(time.Now().Add(-backfillWindow))
	w.lastMetric, w.lastAvail = start, start

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

	payload, newestMetric, newestAvail, err := w.buildPayload()
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

	w.lastMetric, w.lastAvail = newestMetric, newestAvail
	w.Log.Debug("sync round complete", "metrics", len(payload.Metrics), "peers", len(peers))
	return nil
}

func (w *Sync) push(ctx context.Context, target model.Server, payload model.SyncPayload, timestamp model.Time) {
	ok, latencyMs, status := w.Client.PushSync(ctx, target.URL, payload)

	if err := w.Store.InsertAvailability(w.ServerID, []model.Availability{{
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
func (w *Sync) buildPayload() (model.SyncPayload, model.Time, model.Time, error) {
	metrics, err := w.Store.MetricsSince(w.ServerID, w.lastMetric)
	if err != nil {
		return model.SyncPayload{}, w.lastMetric, w.lastAvail, err
	}

	availRows, err := w.Store.AvailabilitySince(w.lastAvail)
	if err != nil {
		return model.SyncPayload{}, w.lastMetric, w.lastAvail, err
	}

	newestMetric := w.lastMetric
	for _, m := range metrics {
		if m.Timestamp.After(newestMetric.Time) {
			newestMetric = m.Timestamp
		}
	}

	newestAvail := w.lastAvail
	availability := make([]model.Availability, 0, len(availRows))
	for _, r := range availRows {
		// Only observations this server made are ours to forward; relaying a
		// peer's records would duplicate them across the mesh.
		if r.FromServerID != w.ServerID {
			continue
		}
		availability = append(availability, model.Availability{
			ToServerID:  r.ToServerID,
			Timestamp:   r.Timestamp,
			IsAvailable: r.IsAvailable,
			LatencyMs:   r.LatencyMs,
			HTTPStatus:  r.HTTPStatus,
		})
		if r.Timestamp.After(newestAvail.Time) {
			newestAvail = r.Timestamp
		}
	}

	return model.SyncPayload{
		ServerID:     w.ServerID,
		ServerName:   w.ServerName,
		Location:     w.Location,
		SelfURL:      w.PublicURL,
		Metrics:      metrics,
		Availability: availability,
	}, newestMetric, newestAvail, nil
}
