package worker

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/series"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

func rollupFixture(t *testing.T) (*store.Store, *Rollup, int64) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	self, err := s.UpsertSelf("11111111-1111-4111-8111-111111111111", "here", "Lab", "", false)
	if err != nil {
		t.Fatal(err)
	}

	w := &Rollup{Store: s, Log: slog.New(slog.DiscardHandler)}
	w.lastPrune = map[series.Level]time.Time{}
	return s, w, self
}

// A pass must only build what can no longer change. The bucket the clock is
// currently in is still filling, and one published half-full could never be
// corrected: the high-water mark it travels under only moves forward.
func TestRollupSealsOnlyWhatIsFinished(t *testing.T) {
	s, w, self := rollupFixture(t)

	now := time.Now()
	// One reading well inside the grace, and one comfortably outside it.
	fresh := model.At(now.Add(-time.Second))
	settled := model.At(now.Add(-series.SealGrace - 5*time.Minute))

	if err := s.RecordSnapshots(self, []model.Metric{
		{Timestamp: settled, CpuPercent: 10},
		{Timestamp: fresh, CpuPercent: 90},
	}); err != nil {
		t.Fatal(err)
	}

	if err := w.pass(now); err != nil {
		t.Fatal(err)
	}

	settledBucket := series.Bucket(settled.DB(), series.L30s)
	if got, err := s.MetricsAtLevel(self, series.L30s, settledBucket, settledBucket); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 || got[0].CpuPercent != 10 {
		t.Errorf("the settled bucket came out as %+v, want one reading of 10", got)
	}

	freshBucket := series.Bucket(fresh.DB(), series.L30s)
	if got, err := s.MetricsAtLevel(self, series.L30s, freshBucket, freshBucket); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Errorf("a bucket the clock is still inside was sealed: %+v", got)
	}
}

// Falling behind has to cost a rescan and nothing else. This is what lets the
// watermark be a hint: a node that was down, or whose Meta row was lost, picks
// the ladder back up from whatever it still holds.
func TestRollupCatchesUpAfterAGap(t *testing.T) {
	s, w, self := rollupFixture(t)

	now := time.Now()
	start := now.Add(-series.SealGrace - 10*time.Minute)

	metrics := make([]model.Metric, 0, 60)
	for i := range 60 {
		metrics = append(metrics, model.Metric{
			Timestamp:  model.At(start.Add(time.Duration(i) * 5 * time.Second)),
			CpuPercent: 25,
		})
	}
	if err := s.RecordSnapshots(self, metrics); err != nil {
		t.Fatal(err)
	}

	// No watermark at all, as after the Meta row was lost.
	if err := w.pass(now); err != nil {
		t.Fatal(err)
	}

	from := series.Bucket(model.At(start).DB(), series.L30s)
	to := series.Bucket(model.At(start.Add(5*time.Minute)).DB(), series.L30s)
	got, err := s.MetricsAtLevel(self, series.L30s, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 9 {
		t.Fatalf("five minutes of readings produced %d buckets, want ten", len(got))
	}
	for _, m := range got {
		if m.CpuPercent != 25 {
			t.Errorf("a bucket came out at %v, want 25", m.CpuPercent)
		}
	}

	// And a second pass over the same ground changes nothing.
	if err := w.pass(now); err != nil {
		t.Fatal(err)
	}
	again, err := s.MetricsAtLevel(self, series.L30s, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(got) {
		t.Errorf("a repeated pass turned %d buckets into %d", len(got), len(again))
	}
}

// The tables the first build stored readings in are dropped once the grace has
// passed - and the node has to go on working without them, because by then
// every reading it serves comes from somewhere else.
func TestLegacyTablesAreRetiredAfterTheGrace(t *testing.T) {
	s, w, self := rollupFixture(t)

	if !s.LegacyActive() {
		t.Fatal("a database upgraded from the first build should still carry its tables")
	}

	// Nothing to retire yet: the window has only just opened.
	if err := w.retire(model.Now()); err != nil {
		t.Fatal(err)
	}
	if !s.LegacyActive() {
		t.Fatal("the tables were dropped before the grace had passed")
	}

	// Wind the stamp back past the grace, the way waiting two days would.
	aged := model.At(time.Now().Add(-legacyGrace - time.Hour))
	if err := s.MetaSet("series.since", aged.DB()); err != nil {
		t.Fatal(err)
	}
	if err := w.retire(model.Now()); err != nil {
		t.Fatal(err)
	}
	if s.LegacyActive() {
		t.Fatal("the grace passed and the tables are still being written")
	}

	// Sampling has to go on working, and go on being readable.
	at := model.Now()
	if _, err := s.InsertMetrics(self, []model.Metric{{
		Timestamp: at, CpuPercent: 33, MemoryPercent: 44,
		Disks: []model.Disk{{Name: "/", TotalGb: 100, UsedGb: 20}},
	}}); err != nil {
		t.Fatalf("storing a reading after the tables were dropped: %v", err)
	}
	latest, err := s.LatestMetricSeries(self)
	if err != nil || latest == nil {
		t.Fatalf("reading it back: %+v %v", latest, err)
	}
	if latest.CpuPercent != 33 || len(latest.Disks) != 1 {
		t.Errorf("the reading came back as %+v", latest)
	}

	// Retention still has a table of its own to sweep, and must not trip over
	// the three that are gone.
	if err := s.Prune(model.At(time.Now().Add(-24*time.Hour)), model.At(time.Now().Add(-7*24*time.Hour))); err != nil {
		t.Errorf("pruning after retirement: %v", err)
	}
}

// Reopening a retired database must not recreate the tables - which is the
// whole reason they are not in the schema every start runs.
func TestARetiredDatabaseStaysRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")

	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.MetaSet("series.since",
		model.At(time.Now().Add(-legacyGrace-time.Hour)).DB()); err != nil {
		t.Fatal(err)
	}
	if err := first.RetireLegacy(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening a retired database: %v", err)
	}
	defer again.Close()

	if again.LegacyActive() {
		t.Error("the tables were created again on the next start")
	}
}
