package store

import (
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/series"
)

const (
	selfUID = "aaaaaaaa-1111-4111-8111-111111111111"
	peerUID = "bbbbbbbb-2222-4222-8222-222222222222"
)

// mesh registers a node and one peer and hands back their row ids.
func mesh(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	s := open(t)

	self, err := s.UpsertSelf(selfUID, "self", "Lab", "http://self:5001", false)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := s.UpsertPeer(peerUID, "peer", "Lab", "http://peer:5001", false, model.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s, self, peer
}

// base is a fixed instant on a bucket boundary at every level of the ladder,
// so that a test's arithmetic is not at the mercy of when it runs.
var base = model.At(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))

// TestRollSamplesComputesTheThreeCounters is the whole reason the schema keeps
// three of them rather than a flag: a bucket has to be able to say how many
// checks it holds, how many succeeded, and how many produced a number, and
// those are three different answers on the same route.
func TestRollSamplesComputesTheThreeCounters(t *testing.T) {
	s, self, peer := mesh(t)

	// Six checks inside one thirty-second bucket: four reachable and timed,
	// one reachable but never timed - which is what a push-only peer looks
	// like - and one that failed outright.
	latencies := []*float64{f(10), f(20), f(30), f(40), nil, nil}
	ok := []bool{true, true, true, true, true, false}

	records := make([]Observation, 0, len(latencies))
	for i := range latencies {
		records = append(records, Observation{
			ToServerID:  peer,
			Timestamp:   model.At(base.Add(time.Duration(i) * time.Second)),
			IsAvailable: ok[i],
			LatencyMs:   latencies[i],
		})
	}
	if err := s.RecordProbes(self, records); err != nil {
		t.Fatal(err)
	}

	bucket := series.Bucket(base.DB(), series.L30s)
	if _, err := s.RollSamples(bucket, bucket+1); err != nil {
		t.Fatal(err)
	}

	points, err := s.EdgeAtLevel(self, peer, series.L30s, bucket, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d buckets, want the six checks folded into one", len(points))
	}

	p := points[0]
	if want := 5.0 / 6.0; p.Availability != want {
		t.Errorf("availability = %v, want %v: five of six checks succeeded", p.Availability, want)
	}
	// The mean is over the four that were actually timed. Dividing by the five
	// successes would dilute it, and dividing by all six would count the
	// failure as a fast reply.
	if p.LatencyMs == nil || *p.LatencyMs != 25 {
		t.Errorf("mean latency = %v, want 25: the mean of the four timed checks", p.LatencyMs)
	}
	if p.MaxLatencyMs == nil || *p.MaxLatencyMs != 40 {
		t.Errorf("peak latency = %v, want 40", p.MaxLatencyMs)
	}
}

// TestFailedProbesCarryNoLatency guards the one number that would otherwise
// poison every latency chart: a probe that timed out records how long this
// node waited, which says nothing about the route.
func TestFailedProbesCarryNoLatency(t *testing.T) {
	s, self, peer := mesh(t)

	if err := s.RecordProbes(self, []Observation{{
		ToServerID:  peer,
		Timestamp:   base,
		IsAvailable: false,
		LatencyMs:   f(10000),
	}}); err != nil {
		t.Fatal(err)
	}

	bucket := series.Bucket(base.DB(), series.L30s)
	if _, err := s.RollSamples(bucket, bucket+1); err != nil {
		t.Fatal(err)
	}

	points, err := s.EdgeAtLevel(self, peer, series.L30s, bucket, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d buckets, want 1", len(points))
	}
	if points[0].LatencyMs != nil {
		t.Errorf("latency = %v, want none: the ten seconds are the timeout, not the route", *points[0].LatencyMs)
	}
	if points[0].Availability != 0 {
		t.Errorf("availability = %v, want 0", points[0].Availability)
	}
}

// TestRollLevelIsExact is what the stored sum buys. Folding ten thirty-second
// buckets into one five-minute bucket has to give the same mean as averaging
// the original readings, whatever the buckets held - and unequal buckets are
// exactly where an average of averages goes wrong.
func TestRollLevelIsExact(t *testing.T) {
	s, self, peer := mesh(t)

	// Two buckets with very different fill: one check at 100ms, then nine at
	// 10ms. The mean of the ten readings is 19; the mean of the two bucket
	// means would be 55.
	records := []Observation{{
		ToServerID: peer, Timestamp: base, IsAvailable: true, LatencyMs: f(100),
	}}
	for i := range 9 {
		records = append(records, Observation{
			ToServerID:  peer,
			Timestamp:   model.At(base.Add(30*time.Second + time.Duration(i)*time.Second)),
			IsAvailable: true,
			LatencyMs:   f(10),
		})
	}
	if err := s.RecordProbes(self, records); err != nil {
		t.Fatal(err)
	}

	fine := series.Bucket(base.DB(), series.L30s)
	if _, err := s.RollSamples(fine, fine+2); err != nil {
		t.Fatal(err)
	}

	coarse := series.Bucket(base.DB(), series.L5m)
	if _, err := s.RollLevel(series.L30s, series.L5m, coarse, coarse+1); err != nil {
		t.Fatal(err)
	}

	points, err := s.EdgeAtLevel(self, peer, series.L5m, coarse, coarse)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d buckets, want 1", len(points))
	}
	if got := *points[0].LatencyMs; got != 19 {
		t.Errorf("mean latency = %v, want 19: an average of the two bucket averages would be 55", got)
	}
}

// TestRollingTwiceChangesNothing is what lets the watermark be a hint. A pass
// that repeats a range it has already built must not double any counter.
func TestRollingTwiceChangesNothing(t *testing.T) {
	s, self, peer := mesh(t)

	records := []Observation{}
	for i := range 6 {
		records = append(records, Observation{
			ToServerID:  peer,
			Timestamp:   model.At(base.Add(time.Duration(i) * time.Second)),
			IsAvailable: true,
			LatencyMs:   f(20),
		})
	}
	if err := s.RecordProbes(self, records); err != nil {
		t.Fatal(err)
	}

	bucket := series.Bucket(base.DB(), series.L30s)
	for range 3 {
		if _, err := s.RollSamples(bucket, bucket+1); err != nil {
			t.Fatal(err)
		}
	}

	points, err := s.EdgeAtLevel(self, peer, series.L30s, bucket, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d buckets, want 1", len(points))
	}
	if *points[0].LatencyMs != 20 || points[0].Availability != 1 {
		t.Errorf("three passes gave latency=%v availability=%v, want 20 and 1",
			*points[0].LatencyMs, points[0].Availability)
	}
}

// TestSnapshotsSurviveTheRoundTrip checks that a whole-machine sample comes
// back out of the long form as the struct that went in, drives and all - and
// that the two numbers not stored are reconstructed rather than lost.
func TestSnapshotsSurviveTheRoundTrip(t *testing.T) {
	s, self, _ := mesh(t)

	in := model.Metric{
		Timestamp:     base,
		CpuPercent:    12.5,
		CpuTempC:      f(48.5),
		MemoryPercent: 61.25,
		MemoryTotalMb: 32768,
		MemoryUsedMb:  20070,
		UptimeSeconds: 864000,
		Disks: []model.Disk{
			{Name: "C:", TotalGb: 931.5, UsedGb: 700.25, TempC: f(37)},
			{Name: "D:", TotalGb: 1863, UsedGb: 100},
		},
	}
	if err := s.RecordSnapshots(self, []model.Metric{in}); err != nil {
		t.Fatal(err)
	}

	out, err := s.MetricsRaw(self, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d samples, want 1", len(out))
	}

	got := out[0]
	if got.CpuPercent != in.CpuPercent || got.MemoryPercent != in.MemoryPercent ||
		got.MemoryUsedMb != in.MemoryUsedMb || got.MemoryTotalMb != in.MemoryTotalMb ||
		got.UptimeSeconds != in.UptimeSeconds {
		t.Errorf("host readings came back as %+v, want %+v", got, in)
	}
	if got.CpuTempC == nil || *got.CpuTempC != *in.CpuTempC {
		t.Errorf("cpu temperature = %v, want %v", got.CpuTempC, *in.CpuTempC)
	}
	if len(got.Disks) != 2 {
		t.Fatalf("got %d drives, want 2", len(got.Disks))
	}
	if got.Disks[0].Name != "C:" || got.Disks[0].TotalGb != 931.5 || got.Disks[0].UsedGb != 700.25 {
		t.Errorf("first drive = %+v", got.Disks[0])
	}
	// Neither of these is stored: they are the other two numbers, and keeping
	// them would be keeping the same fact three times in every row.
	if want := 931.5 - 700.25; got.Disks[0].FreeGb != want {
		t.Errorf("free space = %v, want %v", got.Disks[0].FreeGb, want)
	}
	if want := 700.25 / 931.5 * 100; got.Disks[0].UsagePercent != want {
		t.Errorf("usage = %v, want %v", got.Disks[0].UsagePercent, want)
	}
	// A drive with no sensor has to stay distinguishable from one at zero.
	if got.Disks[1].TempC != nil {
		t.Errorf("second drive reported %v degrees, want no reading", *got.Disks[1].TempC)
	}
}

// TestUptimeTakesTheLastReading guards the one parameter that must not be
// averaged: a counter's mean over a bucket is half a bucket behind the truth.
func TestUptimeTakesTheLastReading(t *testing.T) {
	s, self, _ := mesh(t)

	metrics := []model.Metric{}
	for i := range 6 {
		metrics = append(metrics, model.Metric{
			Timestamp:     model.At(base.Add(time.Duration(i*5) * time.Second)),
			UptimeSeconds: float64(1000 + i*5),
			CpuPercent:    50,
		})
	}
	if err := s.RecordSnapshots(self, metrics); err != nil {
		t.Fatal(err)
	}

	bucket := series.Bucket(base.DB(), series.L30s)
	if _, err := s.RollSamples(bucket, bucket+1); err != nil {
		t.Fatal(err)
	}

	out, err := s.MetricsAtLevel(self, series.L30s, bucket, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d buckets, want 1", len(out))
	}
	if out[0].UptimeSeconds != 1025 {
		t.Errorf("uptime = %v, want 1025 - the newest reading, not the mean of the run", out[0].UptimeSeconds)
	}
	if out[0].CpuPercent != 50 {
		t.Errorf("cpu = %v, want 50: an ordinary reading is still averaged", out[0].CpuPercent)
	}
}

// TestRollupSkipsPeersThatSendTheirOwn is what keeps a node from replacing a
// peer's sealed bucket with the fraction of it that happened to arrive here.
func TestRollupSkipsPeersThatSendTheirOwn(t *testing.T) {
	s, self, peer := mesh(t)

	if err := s.SetProtocol(peer, 2); err != nil {
		t.Fatal(err)
	}

	// A handful of the peer's own readings, as its pushes would deliver them.
	if err := s.RecordSnapshots(peer, []model.Metric{{
		Timestamp: base, CpuPercent: 90, MemoryPercent: 90,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSnapshots(self, []model.Metric{{
		Timestamp: base, CpuPercent: 10, MemoryPercent: 10,
	}}); err != nil {
		t.Fatal(err)
	}

	bucket := series.Bucket(base.DB(), series.L30s)
	if _, err := s.RollSamples(bucket, bucket+1); err != nil {
		t.Fatal(err)
	}

	mine, err := s.MetricsAtLevel(self, series.L30s, bucket, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Fatalf("this node's own readings were not rolled up: got %d buckets", len(mine))
	}

	theirs, err := s.MetricsAtLevel(peer, series.L30s, bucket, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 0 {
		t.Errorf("a peer that seals its own buckets had one computed here: %+v", theirs)
	}
}

func f(v float64) *float64 { return &v }
