// Package worker holds the background loops: local metric collection and
// outbound peer synchronisation.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/metrics"
	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// pruneInterval is how often retention is enforced. Sampling ticks far more
// often than this, and the delete scans both tables in full, so running it on
// every sample would cost more than the extra minute of history it trims.
const pruneInterval = time.Minute

// maxLegacyRetention caps how long the tables the first build stored readings
// in are allowed to keep, whatever the configuration says.
//
// Nothing reads them: the dashboard, the alerts and the peer protocol are all
// answered from the series store. They are written only so that a build rolled
// back within the grace period finds its history, and a day of it is more than
// that needs - the ladder is what keeps the year.
const maxLegacyRetention = 24 * time.Hour

// Metrics samples the local host on a fixed interval and enforces retention.
type Metrics struct {
	Store     *store.Store
	Collector *metrics.Collector
	ServerID  int64
	Interval  time.Duration
	Retention time.Duration
	Log       *slog.Logger

	lastPrune time.Time
}

// Run collects until ctx is cancelled.
func (w *Metrics) Run(ctx context.Context) {
	w.Log.Info("metric collection started", "interval", w.Interval)

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		if err := w.collect(); err != nil {
			w.Log.Error("collect metrics", "err", err)
		}
		if err := w.prune(); err != nil {
			w.Log.Error("prune old records", "err", err)
		}

		select {
		case <-ctx.Done():
			w.Log.Info("metric collection stopped")
			return
		case <-ticker.C:
		}
	}
}

func (w *Metrics) collect() error {
	snap := w.Collector.Collect()

	now := model.Now()
	if _, err := w.Store.InsertMetrics(w.ServerID, []model.Metric{{
		Timestamp:     now,
		CpuPercent:    snap.CpuPercent,
		CpuTempC:      snap.CpuTempC,
		MemoryPercent: snap.MemoryPercent,
		MemoryTotalMb: snap.MemoryTotalMb,
		MemoryUsedMb:  snap.MemoryUsedMb,
		UptimeSeconds: snap.UptimeSeconds,
		Disks:         snap.Disks,
	}}); err != nil {
		return err
	}

	if err := w.Store.TouchLastSeen(w.ServerID, now); err != nil {
		return err
	}

	w.Log.Debug("metrics stored", "cpu", snap.CpuPercent, "mem", snap.MemoryPercent)
	return nil
}

func (w *Metrics) prune() error {
	if w.Retention <= 0 {
		return nil
	}
	now := time.Now()
	if !w.lastPrune.IsZero() && now.Sub(w.lastPrune) < pruneInterval {
		return nil
	}
	w.lastPrune = now
	return w.Store.Prune(
		model.At(now.Add(-min(w.Retention, maxLegacyRetention))),
		model.At(now.Add(-w.Retention)))
}
