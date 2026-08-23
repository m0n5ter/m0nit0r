package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
)

// legacySchema is MetricSnapshots as builds before the disk split created it:
// the volumes of a sample encoded as a JSON document in a column that is NOT
// NULL. Nothing fills that column now, so every insert against such a database
// would fail, and a node would keep running while storing nothing.
const legacySchema = `
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

// There is no migration off that schema by design, so the contract is that the
// file is refused, loudly, and left untouched for its owner to delete.
func TestOpenRefusesADatabaseThatStoresDisksAsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")

	existing, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := existing.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open accepted a database whose MetricSnapshots still carries DisksJson")
	}
	if !strings.Contains(err.Error(), "MetricDisks") {
		t.Errorf("error is %q, want it to name the table that replaced the column", err)
	}

	// Refused, not rewritten: an operator who reads the message and decides to
	// keep the file has to still have it.
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	var n int
	if err := reopened.QueryRow(
		`SELECT COUNT(*) FROM "sqlite_master" WHERE "type"='table' AND "name"='MetricDisks'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the refused database was given a MetricDisks table anyway")
	}
}

// TestOpenIsIdempotent covers the ordinary restart, where every table the
// schema declares is already there.
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
			Disks:         []model.Disk{{Name: "/", TotalGb: 100, UsedGb: float64(i), UsagePercent: float64(i)}},
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
	if len(got[0].Disks) != 1 || got[0].Disks[0].UsedGb != 11 {
		t.Errorf("disks are %+v, want the single volume of the newest sample in the bucket, at 11 GB used", got[0].Disks)
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
