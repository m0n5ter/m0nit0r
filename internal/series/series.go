// Package series defines the shape of the unified metric store: what a
// parameter is, what levels of aggregation exist, and how a reading turns into
// the integer a row holds.
//
// Everything here is a property of the code that reads the rows back, not of
// the data, so none of it is written to the database. A parameter is one byte
// in a row and its meaning lives in this file; the same goes for a level. That
// is deliberate: a build that adds a parameter must not have to migrate a
// dimension table before it can store one.
package series

import "time"

// Param identifies what a row measures.
//
// The values are written into every row, so they are assigned explicitly and
// never reordered: a database written by one build is read by the next.
type Param uint8

const (
	CPUPercent    Param = 0
	CPUTempC      Param = 1
	MemoryPercent Param = 2
	MemoryTotalMb Param = 3
	MemoryUsedMb  Param = 4
	UptimeSeconds Param = 5
	DiskTotalGb   Param = 6
	DiskUsedGb    Param = 7
	DiskTempC     Param = 8
	LatencyMs     Param = 9
)

// Kind says what a parameter's second dimension means, which is what keeps the
// Server2 and Volume columns from being read as the wrong thing.
type Kind uint8

const (
	// Host measures the machine itself: neither second dimension is used.
	Host Kind = iota
	// Disk measures one volume on the machine, named by the Volume column.
	Disk
	// Edge measures the route from Server1 to Server2.
	Edge
)

// Rule says how a bucket's stored aggregate answers "the value here".
type Rule uint8

const (
	// Mean is the average over the bucket, which is what a reading that
	// fluctuates around a level wants.
	Mean Rule = iota
	// Last is the newest reading in the bucket. Uptime is a counter, and the
	// average of a counter over thirty seconds is fifteen seconds behind the
	// truth for no reason. Within a bucket it only ever rises, so the maximum
	// is the newest - and across a reboot the maximum is the value before it,
	// which is wrong for one bucket out of a machine's lifetime.
	Last
)

// Spec is everything the rest of the code needs to know about a parameter.
//
// Scale is what a reading is multiplied by to become the integer stored.
// SQLite spends eight bytes on every REAL whatever its value, and one to eight
// on an integer according to its magnitude, so a percentage kept to a
// hundredth costs two bytes where the float cost eight. The factors are chosen
// to be finer than the sensors they carry: a CPU percentage is not meaningful
// past a hundredth, and a disk is not measured to better than ten megabytes.
type Spec struct {
	Kind  Kind
	Rule  Rule
	Scale float64
	Name  string
}

var specs = map[Param]Spec{
	CPUPercent:    {Host, Mean, 100, "cpuPercent"},
	CPUTempC:      {Host, Mean, 10, "cpuTempC"},
	MemoryPercent: {Host, Mean, 100, "memoryPercent"},
	MemoryTotalMb: {Host, Mean, 1, "memoryTotalMb"},
	MemoryUsedMb:  {Host, Mean, 1, "memoryUsedMb"},
	UptimeSeconds: {Host, Last, 1, "uptimeSeconds"},
	DiskTotalGb:   {Disk, Mean, 100, "diskTotalGb"},
	DiskUsedGb:    {Disk, Mean, 100, "diskUsedGb"},
	DiskTempC:     {Disk, Mean, 10, "diskTempC"},
	LatencyMs:     {Edge, Mean, 100, "latencyMs"},
}

// Of returns a parameter's specification. The bool is false for a byte this
// build does not recognise, which is what a row written by a newer node in the
// mesh looks like: it is stored and ignored rather than rejected.
func Of(p Param) (Spec, bool) {
	spec, ok := specs[p]
	return spec, ok
}

// Encode turns a reading into the integer a row holds.
func Encode(p Param, v float64) int64 {
	scale := specs[p].Scale
	if v < 0 {
		return int64(v*scale - 0.5)
	}
	return int64(v*scale + 0.5)
}

// Decode turns a stored integer back into a reading.
func Decode(p Param, v int64) float64 {
	return float64(v) / specs[p].Scale
}

// DecodeSum turns a stored sum of n readings back into their mean.
func DecodeSum(p Param, sum int64, n int64) float64 {
	if n <= 0 {
		return 0
	}
	return float64(sum) / float64(n) / specs[p].Scale
}

// Level identifies one rung of the aggregation ladder. Like Param it is a byte
// in every row, so the values are fixed.
type Level uint8

const (
	L30s Level = 0
	L5m  Level = 1
	L1h  Level = 2
	L1d  Level = 3
)

// Rung is one level's width and how long rows at that width are kept.
type Rung struct {
	Level  Level
	Width  time.Duration
	Retain time.Duration
}

// Ladder is every level, coarsest last. Each rung is built from the one before
// it, so the order is also the order they are rolled up in.
//
// The widths multiply cleanly - thirty seconds into five minutes into an hour
// into a day - so a coarse bucket is always exactly a whole number of fine
// ones and no reading is ever split between two.
var Ladder = []Rung{
	{L30s, 30 * time.Second, 6 * time.Hour},
	{L5m, 5 * time.Minute, 48 * time.Hour},
	{L1h, time.Hour, 30 * 24 * time.Hour},
	{L1d, 24 * time.Hour, 365 * 24 * time.Hour},
}

// Width returns how wide one bucket at a level is.
func Width(l Level) time.Duration {
	for _, r := range Ladder {
		if r.Level == l {
			return r.Width
		}
	}
	return 0
}

// Retain returns how long rows at a level are kept.
func Retain(l Level) time.Duration {
	for _, r := range Ladder {
		if r.Level == l {
			return r.Retain
		}
	}
	return 0
}

// SampleRetention is how long raw samples are kept.
//
// It is not "a few minutes": it has to outlast the grace the roll-up waits
// before sealing a bucket, which in turn has to outlast the window a peer
// backfills after a restart. A sample deleted before its bucket is sealed is a
// gap in the history that nothing fills in.
const SampleRetention = 20 * time.Minute

// SealGrace is how long after a bucket ends the roll-up waits before treating
// it as complete. A peer's readings arrive over the ordinary sync, which after
// a restart replays the last ten minutes, so a bucket is not finished when its
// last second passes - only when nothing more can still arrive for it.
const SealGrace = 12 * time.Minute

// maxPoints is roughly how many points a chart should have to draw. Past this
// a line is denser than the plot is wide, so the rest cost time without
// showing anything.
//
// Twice what the query-time bucketing this replaced aimed at, because the
// choice is now between fixed widths rather than any width at all: a budget of
// three hundred and sixty would push a six-hour range off the thirty-second
// rung and onto the five-minute one, losing an order of magnitude of detail to
// save points a browser draws without noticing.
const maxPoints = 720

// LevelFor picks the rung that renders a window of this length at about
// maxPoints. The bool is false when raw samples are the right answer, which is
// the live view - the one place the sampling cadence is the point.
//
// This replaces choosing a GROUP BY width at query time: the aggregate has
// already been computed, so the choice is only which one to read.
func LevelFor(window time.Duration) (Level, bool) {
	if window <= time.Hour {
		return 0, false
	}
	for _, r := range Ladder {
		if window/maxPoints <= r.Width {
			return r.Level, true
		}
	}
	return L1d, true
}

// Bucket numbers the bucket a timestamp falls in, as whole widths since the
// epoch. Rows store this rather than a millisecond count: it is the same
// ordering in a fifth of the bytes, and it makes two nodes agree on bucket
// boundaries without having to agree on anything else.
func Bucket(ms int64, l Level) int64 {
	w := Width(l).Milliseconds()
	if w <= 0 {
		return 0
	}
	// Floor division, so that the rare pre-epoch timestamp of an unset clock
	// does not land a bucket ahead of where it belongs.
	if ms < 0 {
		return -((-ms + w - 1) / w)
	}
	return ms / w
}

// BucketStart is the first millisecond covered by a bucket.
func BucketStart(bucket int64, l Level) int64 { return bucket * Width(l).Milliseconds() }

// BucketEnd is the first millisecond after a bucket.
func BucketEnd(bucket int64, l Level) int64 { return (bucket + 1) * Width(l).Milliseconds() }

// SealedThrough is the newest bucket at a level that can no longer change,
// given the current time. Buckets after it are still open and are not rolled
// up, because publishing a half-filled bucket would put a value in the mesh
// that the watermark it travels under can never correct.
func SealedThrough(nowMs int64, l Level) int64 {
	return Bucket(nowMs-SealGrace.Milliseconds(), l) - 1
}
