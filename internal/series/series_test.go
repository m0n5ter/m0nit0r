package series

import (
	"testing"
	"time"
)

// The dashboard's range presets each have to land on a rung that is fine
// enough to be worth drawing and coarse enough to stay inside the point
// budget. The hour is exempt by design: it is the live view, where every
// reading is the point.
func TestLevelForKeepsThePresetsDrawable(t *testing.T) {
	for _, tc := range []struct {
		hours int
		want  Level
		raw   bool
	}{
		{1, 0, true},
		{6, L30s, false},
		{24, L5m, false},
		{72, L1h, false},
		{168, L1h, false},
	} {
		window := time.Duration(tc.hours) * time.Hour

		level, aggregated := LevelFor(window)
		if aggregated == tc.raw {
			t.Errorf("LevelFor(%dh) aggregated=%v, want raw=%v", tc.hours, aggregated, tc.raw)
			continue
		}
		if !aggregated {
			continue
		}
		if level != tc.want {
			t.Errorf("LevelFor(%dh) = level %d, want %d", tc.hours, level, tc.want)
			continue
		}
		if points := int(window / Width(level)); points > maxPoints {
			t.Errorf("%dh at level %d is %d points, over the %d budget",
				tc.hours, level, points, maxPoints)
		}
		// A preset may only choose a rung that still holds the whole window;
		// a chart drawn from rows retention has already deleted would simply
		// start partway along.
		if Retain(level) < window {
			t.Errorf("%dh is drawn from level %d, which only keeps %v",
				tc.hours, level, Retain(level))
		}
	}
}

// The ladder folds one rung into the next by integer division of the bucket
// number, which is only the same thing as dividing the timestamp when each
// width divides the one above it exactly.
func TestLadderWidthsNest(t *testing.T) {
	for i := 1; i < len(Ladder); i++ {
		fine, coarse := Ladder[i-1], Ladder[i]
		if coarse.Width%fine.Width != 0 {
			t.Errorf("level %d is %v wide, which level %d's %v does not divide",
				coarse.Level, coarse.Width, fine.Level, fine.Width)
		}
		if coarse.Retain <= fine.Retain {
			t.Errorf("level %d keeps %v, no longer than the finer level %d's %v - "+
				"a coarser rung exists to outlive the one below it",
				coarse.Level, coarse.Retain, fine.Level, fine.Retain)
		}
	}
}

// A bucket is only sealed once nothing more can arrive for it, and what can
// still arrive is a peer's backfill after a restart. Samples have to outlive
// that wait, or a bucket is sealed from readings that were already deleted.
func TestSamplesOutliveTheSeal(t *testing.T) {
	finest := Ladder[0].Width
	if SampleRetention <= SealGrace+finest {
		t.Errorf("samples are kept %v but a bucket is not sealed until %v after it ends; "+
			"the readings would be gone before they were counted",
			SampleRetention, SealGrace+finest)
	}
}

// Encoding is lossy by design - the scale is what turns an eight-byte float
// into a one or two byte integer - so what matters is that it is lossy by less
// than the sensors are accurate.
func TestEncodingKeepsTheResolutionItPromises(t *testing.T) {
	for _, tc := range []struct {
		param     Param
		value     float64
		tolerance float64
	}{
		{CPUPercent, 12.34, 0.005},
		{CPUTempC, 48.5, 0.05},
		{MemoryUsedMb, 20070, 0.5},
		{DiskUsedGb, 700.25, 0.005},
		{LatencyMs, 12.34, 0.005},
		{LatencyMs, 9999.99, 0.005},
	} {
		got := Decode(tc.param, Encode(tc.param, tc.value))
		if diff := got - tc.value; diff > tc.tolerance || diff < -tc.tolerance {
			t.Errorf("%v round-tripped to %v, off by %v (tolerance %v)",
				tc.value, got, diff, tc.tolerance)
		}
	}
}

// Bucket numbering has to floor, not truncate towards zero, or a timestamp
// before the epoch - which is what an unset clock produces - would land in the
// bucket after the one it belongs to.
func TestBucketFloorsBelowTheEpoch(t *testing.T) {
	width := Width(L30s).Milliseconds()

	if got := Bucket(-1, L30s); got != -1 {
		t.Errorf("Bucket(-1) = %d, want -1", got)
	}
	if got := Bucket(-width, L30s); got != -1 {
		t.Errorf("Bucket(-%d) = %d, want -1", width, got)
	}
	if got := Bucket(-width-1, L30s); got != -2 {
		t.Errorf("Bucket(-%d) = %d, want -2", width+1, got)
	}
	if got := Bucket(0, L30s); got != 0 {
		t.Errorf("Bucket(0) = %d, want 0", got)
	}
}
