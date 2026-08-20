package store

import (
	"database/sql"
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
