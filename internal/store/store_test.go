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
// aggregate is that some columns cannot be averaged: a set of drives and a
// monotonic uptime counter have to come from one real sample, and the query
// leans on a SQLite rule about bare columns to get them from the newest one in
// the bucket. That rule is what this covers.
func TestMetricsBucketedAveragesAndKeepsTheNewestSample(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	node, err := s.UpsertSelf("11111111-1111-4111-8111-111111111111", "node-1", "Lab", "")
	if err != nil {
		t.Fatal(err)
	}

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
	if _, err := s.InsertMetrics(node, samples); err != nil {
		t.Fatal(err)
	}

	since := model.At(base.Add(-time.Hour))

	raw, err := s.MetricsBucketed(node, since, 0)
	if err != nil {
		t.Fatalf("bucket of zero: %v", err)
	}
	if len(raw) != 24 {
		t.Errorf("bucket of zero returned %d samples, want the 24 stored", len(raw))
	}

	got, err := s.MetricsBucketed(node, since, time.Minute)
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

// textKeySchema is the schema as builds before the integer keys created it:
// servers keyed by the GUID they publish, every other table repeating that
// GUID, and timestamps as fixed-width ISO text.
const textKeySchema = `
CREATE TABLE "Servers" (
    "Id"       TEXT NOT NULL PRIMARY KEY,
    "Name"     TEXT NOT NULL,
    "Location" TEXT NOT NULL,
    "Url"      TEXT NULL,
    "IsSelf"   INTEGER NOT NULL,
    "LastSeen" TEXT NOT NULL
);
CREATE TABLE "MetricSnapshots" (
    "Id"            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "ServerId"      TEXT NOT NULL,
    "Timestamp"     TEXT NOT NULL,
    "CpuPercent"    REAL NOT NULL,
    "CpuTempC"      REAL NULL,
    "MemoryPercent" REAL NOT NULL,
    "MemoryTotalMb" REAL NOT NULL,
    "MemoryUsedMb"  REAL NOT NULL,
    "UptimeSeconds" REAL NOT NULL
);
CREATE INDEX "IX_MetricSnapshots_ServerId_Timestamp"
    ON "MetricSnapshots" ("ServerId", "Timestamp");
CREATE TABLE "MetricDisks" (
    "Id"           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "MetricId"     INTEGER NOT NULL REFERENCES "MetricSnapshots"("Id") ON DELETE CASCADE,
    "Name"         TEXT NOT NULL,
    "TotalGb"      REAL NOT NULL,
    "UsedGb"       REAL NOT NULL,
    "FreeGb"       REAL NOT NULL,
    "UsagePercent" REAL NOT NULL,
    "TempC"        REAL NULL
);
CREATE INDEX "IX_MetricDisks_MetricId" ON "MetricDisks" ("MetricId");
CREATE TABLE "AvailabilityRecords" (
    "Id"           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "FromServerId" TEXT NOT NULL,
    "ToServerId"   TEXT NOT NULL,
    "Timestamp"    TEXT NOT NULL,
    "IsAvailable"  INTEGER NOT NULL,
    "LatencyMs"    REAL NULL,
    "HttpStatus"   INTEGER NULL
);
`

const (
	uidSelf = "3f2a1c04-9b7e-4f6a-8d21-0c5e7a9b1234"
	uidPeer = "b81d5e60-2c44-4a19-9f03-77ac2e6b8899"
	uidGone = "00000000-0000-4000-8000-000000000000"
)

// writeTextKeyDatabase lays down a database in the old shape with a row in
// every table, including one snapshot belonging to a server the register never
// had, which is the case the migration cannot carry across.
func writeTextKeyDatabase(t *testing.T, path string) {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(textKeySchema); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO "Servers" VALUES (?,?,?,?,?,?)`,
			[]any{uidSelf, "here", "Lab", nil, 1, "2026-08-01 10:00:30.0000000"}},
		{`INSERT INTO "Servers" VALUES (?,?,?,?,?,?)`,
			[]any{uidPeer, "there", "Remote", "http://there:5001", 0, "2026-08-01 10:00:20.0000000"}},

		{`INSERT INTO "MetricSnapshots" VALUES (?,?,?,?,?,?,?,?,?)`,
			[]any{1, uidSelf, "2026-08-01 10:00:05.1230000", 12.5, 41.0, 30.0, 8192.0, 2048.0, 3600.0}},
		{`INSERT INTO "MetricSnapshots" VALUES (?,?,?,?,?,?,?,?,?)`,
			[]any{2, uidGone, "2026-08-01 10:00:06.0000000", 99.0, nil, 99.0, 1024.0, 1000.0, 60.0}},

		{`INSERT INTO "MetricDisks" ("MetricId","Name","TotalGb","UsedGb","FreeGb","UsagePercent","TempC")
		  VALUES (?,?,?,?,?,?,?)`, []any{1, "/", 500.0, 200.0, 300.0, 40.0, 38.0}},
		{`INSERT INTO "MetricDisks" ("MetricId","Name","TotalGb","UsedGb","FreeGb","UsagePercent","TempC")
		  VALUES (?,?,?,?,?,?,?)`, []any{2, "/orphan", 100.0, 1.0, 99.0, 1.0, nil}},

		{`INSERT INTO "AvailabilityRecords" ("FromServerId","ToServerId","Timestamp","IsAvailable","LatencyMs","HttpStatus")
		  VALUES (?,?,?,?,?,?)`, []any{uidSelf, uidPeer, "2026-08-01 10:00:20.5000000", 1, 12.5, 200}},
	} {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
}

// The peer register is what holds the mesh together and a week of history is
// what the charts draw, so opening a database written before the integer keys
// has to carry both across rather than start clean.
func TestOpenMigratesTextIdsAndTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	writeTextKeyDatabase(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open a database with text ids: %v", err)
	}
	defer s.Close()

	// Identity and address survive, and the ids they are keyed by are now the
	// integers the tables reference.
	self, found, err := s.ServerByUID(uidSelf)
	if err != nil || !found {
		t.Fatalf("self after migration: found=%v err=%v", found, err)
	}
	if !self.IsSelf || self.Name != "here" {
		t.Errorf("self is %+v, want the migrated row still marked as this host", self)
	}
	if want := time.Date(2026, 8, 1, 10, 0, 30, 0, time.UTC); !self.LastSeen.Equal(want) {
		t.Errorf("self last seen %s, want %s", self.LastSeen, want)
	}

	peers, err := s.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].UID != uidPeer || peers[0].URL != "http://there:5001" {
		t.Fatalf("peers are %+v, want the one address the register held", peers)
	}

	// A sample keeps its reading, its sub-second stamp and its volumes.
	metric, err := s.LatestMetric(self.ID)
	if err != nil || metric == nil {
		t.Fatalf("latest metric: %+v %v", metric, err)
	}
	if want := time.Date(2026, 8, 1, 10, 0, 5, 123_000_000, time.UTC); !metric.Timestamp.Equal(want) {
		t.Errorf("sample stamped %s, want %s to the millisecond", metric.Timestamp, want)
	}
	if metric.CpuPercent != 12.5 || metric.CpuTempC == nil || *metric.CpuTempC != 41 {
		t.Errorf("sample is %+v, want its readings unchanged", metric)
	}
	if len(metric.Disks) != 1 || metric.Disks[0].Name != "/" || metric.Disks[0].UsedGb != 200 {
		t.Errorf("volumes are %+v, want the one the sample reported", metric.Disks)
	}

	// The observation is still an edge between the same two nodes.
	rows, err := s.AvailabilitySince(model.At(time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].FromUID != uidSelf || rows[0].ToUID != uidPeer {
		t.Fatalf("availability is %+v, want the one edge, both ends resolved", rows)
	}
	if rows[0].LatencyMs == nil || *rows[0].LatencyMs != 12.5 || !rows[0].IsAvailable {
		t.Errorf("observation is %+v, want its reading unchanged", rows[0])
	}

	// A snapshot whose server was never registered has nothing to point at, and
	// its volumes must not be left behind pointing at a row that is gone.
	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM "MetricDisks" WHERE "Name"='/orphan'`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d volumes survived the snapshot they belonged to", orphans)
	}

	// Reopening must not try to migrate what is already migrated.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a migrated database: %v", err)
	}
	defer again.Close()

	migrated, _, err := again.ServerByUID(uidSelf)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := again.ServerByUID(uidPeer); err != nil || !found {
		t.Errorf("peer lost on reopen: found=%v err=%v", found, err)
	}

	// Sampling resumes on top of the carried-over rows. The snapshot ids came
	// across as they were, so a new one must not land on an id whose volumes
	// are already stored against it.
	if _, err := again.InsertMetrics(migrated.ID, []model.Metric{{
		Timestamp:  model.At(time.Date(2026, 8, 1, 10, 1, 0, 0, time.UTC)),
		CpuPercent: 7,
		Disks:      []model.Disk{{Name: "/data", TotalGb: 1000, UsedGb: 10}},
	}}); err != nil {
		t.Fatal(err)
	}

	latest, err := again.LatestMetric(migrated.ID)
	if err != nil || latest == nil {
		t.Fatalf("latest metric after migration: %+v %v", latest, err)
	}
	if len(latest.Disks) != 1 || latest.Disks[0].Name != "/data" {
		t.Errorf("the new sample carries %+v, want only its own volume", latest.Disks)
	}
}

// A peer forwards what it can see, which can include a node this one has not
// been introduced to yet. The reading is worth more than the placeholder costs.
func TestEnsureServerRegistersAnUnknownIdOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first, err := s.EnsureServer(uidGone)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureServer(uidGone)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same id resolved to %d and then %d", first, second)
	}

	// An introduction arriving afterwards names it without adding a second row.
	named, err := s.UpsertPeer(uidGone, "late", "Remote", "http://late:5001", model.Now())
	if err != nil {
		t.Fatal(err)
	}
	if named != first {
		t.Errorf("the introduction landed on row %d, not the %d already standing in", named, first)
	}

	servers, err := s.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "late" {
		t.Errorf("servers are %+v, want the one row, now named", servers)
	}
}
