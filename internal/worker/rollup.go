package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/series"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// rollupInterval is how often the ladder is advanced. It is the width of its
// finest rung, so a pass has at most one new bucket to build and the work is
// spread evenly rather than arriving in batches.
const rollupInterval = 30 * time.Second

// Rollup folds raw samples up the aggregation ladder and enforces each rung's
// retention.
//
// Nothing here is time-critical and nothing here is authoritative: every pass
// recomputes whole buckets from what is stored and writes them over whatever
// was there, so falling behind, restarting, or running the same range twice
// all cost a rescan and change no number. That is what lets the watermarks be
// a hint rather than a ledger.
//
// It aggregates only the series this node is entitled to: its own, and those
// of peers too old to send aggregates of their own. A peer that sends them has
// sealed its buckets from all of its samples, where this node saw only the
// ones that happened to arrive.
type Rollup struct {
	Store *store.Store
	Log   *slog.Logger

	// When each level was last swept. A rung is pruned no more often than it
	// is wide: sweeping the daily rows every thirty seconds would scan a
	// year's worth to delete nothing.
	lastPrune map[series.Level]time.Time

	// When the raw readings were last swept. See samplePruneInterval.
	lastSamplePrune time.Time

	// When drives nothing refers to any more were last forgotten. Its zero
	// value makes that happen on the first pass after a start, which is the
	// one moment a node is likely to be holding names it will never see again.
	lastVolumeSweep time.Time
}

// Run advances the ladder until ctx is cancelled.
func (w *Rollup) Run(ctx context.Context) {
	w.Log.Info("rollup started", "interval", rollupInterval, "levels", len(series.Ladder))
	w.lastPrune = map[series.Level]time.Time{}

	ticker := time.NewTicker(rollupInterval)
	defer ticker.Stop()

	for {
		if err := w.pass(time.Now()); err != nil {
			w.Log.Error("roll up series", "err", err)
		}
		if err := w.prune(time.Now()); err != nil {
			w.Log.Error("prune series", "err", err)
		}
		if err := w.retire(model.Now()); err != nil {
			w.Log.Error("retire compatibility tables", "err", err)
		}

		select {
		case <-ctx.Done():
			w.Log.Info("rollup stopped")
			return
		case <-ticker.C:
		}
	}
}

// watermarkKey names where a level's progress is kept. The level's number is
// in the key rather than the value, so a build that adds a rung does not
// disturb the ones already there.
func watermarkKey(l series.Level) string { return fmt.Sprintf("rollup.level.%d", l) }

// pass advances every rung by whatever has sealed since the last one.
func (w *Rollup) pass(now time.Time) error {
	nowMs := model.At(now).DB()

	for i, rung := range series.Ladder {
		sealed := series.SealedThrough(nowMs, rung.Level)

		from, ok, err := w.startOf(rung.Level, i)
		if err != nil {
			return err
		}
		if !ok || from > sealed {
			// Nothing has been recorded at the level below yet, or nothing
			// new has finished since the last pass.
			continue
		}

		var rows int64
		if i == 0 {
			rows, err = w.Store.RollSamples(from, sealed+1)
		} else {
			rows, err = w.Store.RollLevel(series.Ladder[i-1].Level, rung.Level, from, sealed+1)
		}
		if err != nil {
			return err
		}

		if err := w.Store.MetaSet(watermarkKey(rung.Level), sealed+1); err != nil {
			return err
		}
		if rows > 0 {
			w.Log.Debug("rolled up", "level", rung.Level, "buckets", sealed+1-from, "rows", rows)
		}
	}
	return nil
}

// startOf is the first bucket of a level that has not been built yet.
//
// The stored watermark is the ordinary answer. Where there is none - a fresh
// database, or one whose Meta row was lost - the oldest thing the level is
// built from stands in, which rebuilds everything still available rather than
// silently starting from now and leaving a hole.
func (w *Rollup) startOf(level series.Level, i int) (int64, bool, error) {
	mark, ok, err := w.Store.MetaGet(watermarkKey(level))
	if err != nil {
		return 0, false, err
	}
	if ok {
		return mark, true, nil
	}

	if i == 0 {
		return w.Store.OldestSampleBucket()
	}

	fine := series.Ladder[i-1].Level
	oldest, ok, err := w.Store.OldestRollupBucket(fine)
	if err != nil || !ok {
		return 0, false, err
	}
	// Named in the finer level's buckets; the coarser one counts in its own.
	ratio := series.Width(level).Milliseconds() / series.Width(fine).Milliseconds()
	return oldest / ratio, true, nil
}

// samplePruneInterval is how often the raw readings are swept.
//
// Not every pass. The sweep is by time, which is the last column of that
// table's key, so it walks the table - small in absolute terms, but on a large
// mesh it is still a million rows to delete two per cent of them. Running it
// every few minutes costs a few extra minutes of readings kept and a tenth of
// the work.
const samplePruneInterval = 2 * time.Minute

// prune enforces retention: the raw readings every few minutes, and each rung
// no more often than it is wide.
func (w *Rollup) prune(now time.Time) error {
	if now.Sub(w.lastSamplePrune) >= samplePruneInterval {
		w.lastSamplePrune = now
		if err := w.Store.PruneSamples(model.At(now.Add(-series.SampleRetention))); err != nil {
			return err
		}
	}

	for _, rung := range series.Ladder {
		if last, seen := w.lastPrune[rung.Level]; seen && now.Sub(last) < rung.Width {
			continue
		}
		w.lastPrune[rung.Level] = now

		cutoff := series.Bucket(model.At(now.Add(-rung.Retain)).DB(), rung.Level)
		if err := w.Store.PruneRollups(rung.Level, cutoff); err != nil {
			return err
		}
	}

	// The only thing that ever drops a drive plugged in once and taken away
	// again - but it has to look through every aggregate to be sure nothing
	// still refers to it, so it runs on the slowest schedule there is. A
	// handful of stale names in a dimension table costs nothing in the
	// meantime.
	coarsest := series.Ladder[len(series.Ladder)-1]
	if now.Sub(w.lastVolumeSweep) >= coarsest.Width {
		w.lastVolumeSweep = now
		return w.Store.DropVolumes()
	}
	return nil
}

// legacyGrace is how long the tables the first build stored readings in are
// kept after a node starts writing series instead.
//
// Nothing reads them from the moment this build starts: every chart, the
// matrix, the cards and the alert thresholds come from the series store, and
// the original form of a payload is filled from it too. They are kept for one
// reason - a build rolled back inside this window finds its history where it
// left it - and two days is long enough for the upgrade to have been watched
// through a working day and a night.
const legacyGrace = 48 * time.Hour

// retire drops those tables once the grace has passed.
//
// Deliberately not gated on the peers' versions. What their version decides is
// which form of the payload to send them, which is a question about the
// protocol; these tables are storage, and a peer too old to understand the
// series form is served from the series store just the same. Waiting on a node
// that may never be upgraded would keep several hundred megabytes for nothing.
func (w *Rollup) retire(now model.Time) error {
	if !w.Store.LegacyActive() {
		return nil
	}

	age, known, err := w.Store.LegacyAge(now)
	if err != nil || !known || age < legacyGrace {
		return err
	}

	w.Log.Info("dropping the tables the first build stored readings in",
		"seriesAge", age.Round(time.Hour), "grace", legacyGrace)
	return w.Store.RetireLegacy()
}
