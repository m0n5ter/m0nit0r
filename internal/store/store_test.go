package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
)

// preTemperatureSchema is MetricSnapshots as an earlier build created it. A
// deployed node opens the database it already has, so the upgrade path over a
// table without the temperature column is the one that has to keep working.
const preTemperatureSchema = `
CREATE TABLE "MetricSnapshots" (
    "Id"            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "ServerId"      TEXT NOT NULL,
    "Timestamp"     TEXT NOT NULL,
    "CpuPercent"    REAL NOT NULL,
    "MemoryPercent" REAL NOT NULL,
    "MemoryTotalMb" REAL NOT NULL,
    "MemoryUsedMb"  REAL NOT NULL,
    "UptimeSeconds" REAL NOT NULL,
    "DisksJson"     TEXT NOT NULL
);`

func TestOpenMigratesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")

	existing, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := existing.Exec(preTemperatureSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := existing.Exec(`INSERT INTO "MetricSnapshots"
		("ServerId","Timestamp","CpuPercent","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds","DisksJson")
		VALUES ('node-1','2026-08-01 10:00:00.0000000',11.5,40,1000,400,3600,'[]')`); err != nil {
		t.Fatal(err)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-temperature database: %v", err)
	}
	defer s.Close()

	temp := 61.5
	if _, err := s.InsertMetrics("node-1", []model.Metric{{
		Timestamp:  model.Now(),
		CpuPercent: 22,
		CpuTempC:   &temp,
		DisksJSON:  `[{"name":"/","tempC":38}]`,
	}}); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}

	metrics, err := s.MetricsSince("node-1", model.At(time.Now().Add(-30*24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 2 {
		t.Fatalf("got %d metrics, want the pre-existing row and the new one", len(metrics))
	}

	// The row written before the column existed reads back as no measurement
	// rather than as zero degrees.
	if metrics[0].CpuTempC != nil {
		t.Errorf("pre-existing row: CpuTempC = %v, want nil", *metrics[0].CpuTempC)
	}
	if metrics[1].CpuTempC == nil {
		t.Fatal("new row: CpuTempC = nil, want 61.5")
	}
	if *metrics[1].CpuTempC != temp {
		t.Errorf("new row: CpuTempC = %v, want %v", *metrics[1].CpuTempC, temp)
	}
}

// TestOpenIsIdempotent covers the ordinary restart, where the column the
// migration adds is already there.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	present, err := hasColumn(second.db, "MetricSnapshots", "CpuTempC")
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Error("CpuTempC missing after reopen")
	}
}

// A bucketed query is the dashboard's wide ranges, where the point of the
// aggregate is that some columns cannot be averaged: a JSON document of drives
// and a monotonic uptime counter have to come from one real sample, and the
// query leans on a SQLite rule about bare columns to get them from the newest
// one in the bucket. That rule is what this covers.
func TestMetricsBucketedAveragesAndKeepsTheNewestSample(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	samples := make([]model.Metric, 0, 24)
	for i := range 24 {
		m := model.Metric{
			Timestamp:     model.At(base.Add(time.Duration(i) * 5 * time.Second)),
			CpuPercent:    float64(i),
			UptimeSeconds: float64(100 + i),
			DisksJSON:     fmt.Sprintf(`[{"name":"/","tempC":%d}]`, i),
		}
		// Only the first minute has a temperature, so the second bucket covers
		// the host that reports none: averaging nothing has to stay nil rather
		// than become a zero the chart would draw as a cold drive.
		if i < 12 {
			temp := float64(50 + i)
			m.CpuTempC = &temp
		}
		samples = append(samples, m)
	}
	if _, err := s.InsertMetrics("node-1", samples); err != nil {
		t.Fatal(err)
	}

	since := model.At(base.Add(-time.Hour))

	raw, err := s.MetricsBucketed("node-1", since, 0)
	if err != nil {
		t.Fatalf("bucket of zero: %v", err)
	}
	if len(raw) != 24 {
		t.Errorf("bucket of zero returned %d samples, want the 24 stored", len(raw))
	}

	got, err := s.MetricsBucketed("node-1", since, time.Minute)
	if err != nil {
		t.Fatalf("one-minute buckets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d buckets over two minutes of samples, want 2", len(got))
	}

	// Twelve samples numbered 0..11, then 12..23.
	if got[0].CpuPercent != 5.5 || got[1].CpuPercent != 17.5 {
		t.Errorf("bucket averages are %v and %v, want 5.5 and 17.5", got[0].CpuPercent, got[1].CpuPercent)
	}
	if got[0].UptimeSeconds != 111 {
		t.Errorf("uptime is %v, want 111 from the newest sample in the bucket", got[0].UptimeSeconds)
	}
	if want := `[{"name":"/","tempC":11}]`; got[0].DisksJSON != want {
		t.Errorf("disks are %s, want %s from the newest sample in the bucket", got[0].DisksJSON, want)
	}
	if want := base.Add(55 * time.Second); !got[0].Timestamp.Equal(want) {
		t.Errorf("bucket is stamped %s, want %s, its newest sample", got[0].Timestamp, want)
	}
	if got[0].CpuTempC == nil || *got[0].CpuTempC != 55.5 {
		t.Errorf("temperature is %v, want the 55.5 average", got[0].CpuTempC)
	}
	if got[1].CpuTempC != nil {
		t.Errorf("temperature is %v over samples that reported none, want nil", *got[1].CpuTempC)
	}
}
