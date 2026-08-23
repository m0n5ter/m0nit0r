package api

import (
	"testing"
	"time"
)

// The dashboard's presets are the windows this has to answer for, and what
// matters is the same for each: a bucket a person can name, and few enough
// points that the chart is drawing data rather than pixels. The hour is
// exempt by design - it is the live view, where every sample is the point.
func TestBucketForKeepsThePresetsDrawable(t *testing.T) {
	for _, tc := range []struct {
		hours int
		want  time.Duration
	}{
		{1, 0},
		{6, time.Minute},
		{24, 5 * time.Minute},
		{72, 15 * time.Minute},
		{168, 30 * time.Minute},
	} {
		window := time.Duration(tc.hours) * time.Hour

		got := bucketFor(window)
		if got != tc.want {
			t.Errorf("bucketFor(%dh) = %v, want %v", tc.hours, got, tc.want)
			continue
		}
		if got == 0 {
			continue
		}
		if points := int(window / got); points > maxPoints {
			t.Errorf("%dh in %v buckets is %d points, over the %d budget", tc.hours, got, points, maxPoints)
		}
	}
}
