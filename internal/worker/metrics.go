// Package worker holds the background loops: local metric collection and
// outbound peer synchronisation.
package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/metrics"
	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// Metrics samples the local host on a fixed interval and enforces retention.
type Metrics struct {
	Store     *store.Store
	Collector *metrics.Collector
	ServerID  string
	Interval  time.Duration
	Retention time.Duration
	Log       *slog.Logger
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

	disks, err := json.Marshal(snap.Disks)
	if err != nil {
		return err
	}

	now := model.Now()
	if _, err := w.Store.InsertMetrics(w.ServerID, []model.Metric{{
		Timestamp:     now,
		CpuPercent:    snap.CpuPercent,
		MemoryPercent: snap.MemoryPercent,
		MemoryTotalMb: snap.MemoryTotalMb,
		MemoryUsedMb:  snap.MemoryUsedMb,
		UptimeSeconds: snap.UptimeSeconds,
		DisksJSON:     string(disks),
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
	return w.Store.Prune(model.At(time.Now().Add(-w.Retention)))
}
