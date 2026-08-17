// Package metrics samples local CPU, memory, disk and uptime.
package metrics

import (
	"math"
	"sync"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
)

// primeWindow is how long the very first CPU sample blocks for. Later samples
// use the previous tick as their baseline and block for nothing at all.
const primeWindow = 500 * time.Millisecond

// Collector samples the host. It keeps the previous CPU counter reading so that
// utilisation is measured across the whole interval between ticks rather than
// across an artificial sleep inside each call.
type Collector struct {
	mu        sync.Mutex
	prevIdle  uint64
	prevTotal uint64
	primed    bool
}

// New returns a Collector ready for use.
func New() *Collector { return &Collector{} }

// Collect takes one sample. Individual probes that fail degrade to zero rather
// than failing the whole snapshot, since a host may legitimately expose only
// some of them.
func (c *Collector) Collect() model.Snapshot {
	cpu := c.cpuPercent()
	usedMb, totalMb := memoryMb()
	disks := diskInfo()
	uptime := uptimeSeconds()

	memPct := 0.0
	if totalMb > 0 {
		memPct = round1(usedMb / totalMb * 100)
	}

	if disks == nil {
		disks = []model.Disk{}
	}

	return model.Snapshot{
		CpuPercent:    round1(cpu),
		MemoryUsedMb:  round1(usedMb),
		MemoryTotalMb: round1(totalMb),
		MemoryPercent: memPct,
		UptimeSeconds: uptime,
		Disks:         disks,
	}
}

func (c *Collector) cpuPercent() float64 {
	idle, total, err := cpuTimes()
	if err != nil {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.primed {
		// Nothing to diff against on the first call, so sample a short window
		// instead of reporting a meaningless zero.
		time.Sleep(primeWindow)
		idle2, total2, err := cpuTimes()
		if err != nil {
			return 0
		}
		c.prevIdle, c.prevTotal, c.primed = idle2, total2, true
		return deltaPercent(idle, total, idle2, total2)
	}

	pct := deltaPercent(c.prevIdle, c.prevTotal, idle, total)
	c.prevIdle, c.prevTotal = idle, total
	return pct
}

// deltaPercent converts two readings of the cumulative idle/total counters into
// a utilisation percentage.
func deltaPercent(idle0, total0, idle1, total1 uint64) float64 {
	if total1 <= total0 || idle1 < idle0 {
		return 0
	}
	totalDiff := float64(total1 - total0)
	idleDiff := float64(idle1 - idle0)
	pct := (1 - idleDiff/totalDiff) * 100
	return math.Min(100, math.Max(0, pct))
}

func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

// makeDisk builds a Disk entry from raw byte counts.
func makeDisk(name string, totalBytes, freeBytes uint64) (model.Disk, bool) {
	if totalBytes == 0 {
		return model.Disk{}, false
	}
	const gb = 1024 * 1024 * 1024
	totalGb := float64(totalBytes) / gb
	freeGb := float64(freeBytes) / gb
	usedGb := totalGb - freeGb
	return model.Disk{
		Name:         name,
		TotalGb:      round2(totalGb),
		UsedGb:       round2(usedGb),
		FreeGb:       round2(freeGb),
		UsagePercent: round1(usedGb / totalGb * 100),
	}, true
}
