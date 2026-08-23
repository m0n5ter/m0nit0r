// Package store persists servers, metric samples and availability records in
// SQLite. The schema and the TEXT timestamp encoding match what the previous
// EF Core implementation created, so an existing monitor.db opens unchanged.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS "Servers" (
    "Id"       TEXT NOT NULL PRIMARY KEY,
    "Name"     TEXT NOT NULL,
    "Location" TEXT NOT NULL,
    "Url"      TEXT NULL,
    "IsSelf"   INTEGER NOT NULL,
    "LastSeen" TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS "MetricSnapshots" (
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

CREATE INDEX IF NOT EXISTS "IX_MetricSnapshots_ServerId_Timestamp"
    ON "MetricSnapshots" ("ServerId", "Timestamp");

-- One row per volume in a snapshot. A host's drives come and go - a USB disk
-- appears, an array is unmounted - so they cannot be columns on the snapshot,
-- and a JSON blob in one made them unqueryable and unaggregatable. The
-- reference is declarative: nothing enables foreign_keys, and Prune deletes
-- these rows itself rather than relying on a pragma that a reconnect would
-- silently drop.
CREATE TABLE IF NOT EXISTS "MetricDisks" (
    "Id"           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "MetricId"     INTEGER NOT NULL REFERENCES "MetricSnapshots"("Id") ON DELETE CASCADE,
    "Name"         TEXT NOT NULL,
    "TotalGb"      REAL NOT NULL,
    "UsedGb"       REAL NOT NULL,
    "FreeGb"       REAL NOT NULL,
    "UsagePercent" REAL NOT NULL,
    "TempC"        REAL NULL
);

CREATE INDEX IF NOT EXISTS "IX_MetricDisks_MetricId"
    ON "MetricDisks" ("MetricId");

CREATE TABLE IF NOT EXISTS "AvailabilityRecords" (
    "Id"           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "FromServerId" TEXT NOT NULL,
    "ToServerId"   TEXT NOT NULL,
    "Timestamp"    TEXT NOT NULL,
    "IsAvailable"  INTEGER NOT NULL,
    "LatencyMs"    REAL NULL,
    "HttpStatus"   INTEGER NULL
);

CREATE INDEX IF NOT EXISTS "IX_AvailabilityRecords_FromServerId_ToServerId_Timestamp"
    ON "AvailabilityRecords" ("FromServerId", "ToServerId", "Timestamp");
`

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite file at path and ensures the schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite tolerates exactly one writer. Serialising through a single pooled
	// connection is simpler than handling lock contention, and the query volume
	// here is far too low for the lost parallelism to matter.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=10000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	// Checked before the schema is touched, so a database this build cannot
	// use is left exactly as it was found. One written before the disks moved
	// into their own table still carries them as a NOT NULL column that
	// nothing here fills, and every insert would fail against it - one sample
	// at a time, every few seconds, while the node went on looking healthy.
	//
	// There is deliberately no migration: retention keeps a week at most, so
	// the history is cheap to lose and not worth the code to carry across.
	// Dropping the tables unasked would be worse than refusing, though, so
	// this says what it found and leaves the file for its owner to remove.
	legacy, err := hasColumn(db, "MetricSnapshots", "DisksJson")
	if err != nil {
		db.Close()
		return nil, err
	}
	if legacy {
		db.Close()
		return nil, fmt.Errorf("%s stores disks as JSON, which this build replaced with the "+
			"MetricDisks table; delete the file (and its -wal and -shm) to start clean", path)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Store{db: db}, nil
}

// hasColumn reports whether a table already carries a column, which is how a
// database written by an earlier build is recognised.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE "name"=?`, table, column).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	return n > 0, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// ── Servers ─────────────────────────────────────────────────────────────────

// UpsertSelf writes this instance's own row, preserving any URL already stored.
func (s *Store) UpsertSelf(id, name, location string) error {
	_, err := s.db.Exec(`
		INSERT INTO "Servers" ("Id","Name","Location","Url","IsSelf","LastSeen")
		VALUES (?,?,?,NULL,1,?)
		ON CONFLICT("Id") DO UPDATE SET
			"Name"=excluded."Name",
			"Location"=excluded."Location",
			"IsSelf"=1,
			"LastSeen"=excluded."LastSeen"`,
		id, name, location, model.Now().DB())
	if err != nil {
		return fmt.Errorf("upsert self: %w", err)
	}
	return nil
}

// UpsertPeer records a peer's identity. A blank url leaves any stored URL
// alone, so an introduction that omits it cannot erase a working address.
func (s *Store) UpsertPeer(id, name, location, url string, lastSeen model.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO "Servers" ("Id","Name","Location","Url","IsSelf","LastSeen")
		VALUES (?,?,?,NULLIF(?,''),0,?)
		ON CONFLICT("Id") DO UPDATE SET
			"Name"=excluded."Name",
			"Location"=excluded."Location",
			"Url"=COALESCE(excluded."Url", "Servers"."Url"),
			"LastSeen"=excluded."LastSeen"`,
		id, name, location, url, lastSeen.DB())
	if err != nil {
		return fmt.Errorf("upsert peer %s: %w", id, err)
	}
	return nil
}

// SetPeerURL overwrites a peer's address unconditionally.
func (s *Store) SetPeerURL(id, url string) error {
	_, err := s.db.Exec(`UPDATE "Servers" SET "Url"=NULLIF(?,'') WHERE "Id"=?`, url, id)
	if err != nil {
		return fmt.Errorf("set peer url %s: %w", id, err)
	}
	return nil
}

// TouchLastSeen records that a server was reachable at t.
func (s *Store) TouchLastSeen(id string, t model.Time) error {
	_, err := s.db.Exec(`UPDATE "Servers" SET "LastSeen"=? WHERE "Id"=?`, t.DB(), id)
	if err != nil {
		return fmt.Errorf("touch last seen %s: %w", id, err)
	}
	return nil
}

// GetServer returns one server row. The bool reports whether it existed.
func (s *Store) GetServer(id string) (model.Server, bool, error) {
	row := s.db.QueryRow(`
		SELECT "Id","Name","Location",COALESCE("Url",''),"IsSelf","LastSeen"
		FROM "Servers" WHERE "Id"=?`, id)

	srv, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Server{}, false, nil
	}
	if err != nil {
		return model.Server{}, false, fmt.Errorf("get server %s: %w", id, err)
	}
	return srv, true, nil
}

// ListServers returns every known server, self included.
func (s *Store) ListServers() ([]model.Server, error) {
	return s.queryServers(`
		SELECT "Id","Name","Location",COALESCE("Url",''),"IsSelf","LastSeen"
		FROM "Servers"
		ORDER BY "IsSelf" DESC, "Name"`)
}

// ListPeers returns non-self servers that still have an address configured.
func (s *Store) ListPeers() ([]model.Server, error) {
	return s.queryServers(`
		SELECT "Id","Name","Location",COALESCE("Url",''),"IsSelf","LastSeen"
		FROM "Servers"
		WHERE "IsSelf"=0 AND "Url" IS NOT NULL AND "Url"<>''
		ORDER BY "Name"`)
}

func (s *Store) queryServers(query string, args ...any) ([]model.Server, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query servers: %w", err)
	}
	defer rows.Close()

	servers := []model.Server{}
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		servers = append(servers, srv)
	}
	return servers, rows.Err()
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanServer(sc scanner) (model.Server, error) {
	var (
		srv      model.Server
		lastSeen string
	)
	if err := sc.Scan(&srv.ID, &srv.Name, &srv.Location, &srv.URL, &srv.IsSelf, &lastSeen); err != nil {
		return model.Server{}, err
	}
	t, err := model.ParseDB(lastSeen)
	if err != nil {
		return model.Server{}, err
	}
	srv.LastSeen = t
	return srv, nil
}

// ── Metrics ─────────────────────────────────────────────────────────────────

// InsertMetrics appends samples for serverID and reports how many were
// written. A sample's volumes go in the same transaction, so a snapshot never
// exists without the disks it reported.
func (s *Store) InsertMetrics(serverID string, metrics []model.Metric) (int, error) {
	if len(metrics) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin metrics tx: %w", err)
	}
	defer tx.Rollback()

	snapshots, err := tx.Prepare(`
		INSERT INTO "MetricSnapshots"
			("ServerId","Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds")
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare metric insert: %w", err)
	}
	defer snapshots.Close()

	volumes, err := tx.Prepare(`
		INSERT INTO "MetricDisks"
			("MetricId","Name","TotalGb","UsedGb","FreeGb","UsagePercent","TempC")
		VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare disk insert: %w", err)
	}
	defer volumes.Close()

	for _, m := range metrics {
		res, err := snapshots.Exec(serverID, m.Timestamp.DB(), m.CpuPercent, m.CpuTempC, m.MemoryPercent,
			m.MemoryTotalMb, m.MemoryUsedMb, m.UptimeSeconds)
		if err != nil {
			return 0, fmt.Errorf("insert metric: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("metric id: %w", err)
		}
		for _, disk := range m.Disks {
			if _, err := volumes.Exec(id, disk.Name, disk.TotalGb, disk.UsedGb, disk.FreeGb,
				disk.UsagePercent, disk.TempC); err != nil {
				return 0, fmt.Errorf("insert disk %q: %w", disk.Name, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit metrics: %w", err)
	}
	return len(metrics), nil
}

// MaxMetricTimestamp returns the newest stored sample time for a server. The
// bool is false when the server has no samples yet.
//
// Peers resend an overlapping window on every sync, so this is the high-water
// mark used to discard duplicates. Comparing against one value keeps the cost
// constant regardless of how much history has accumulated.
func (s *Store) MaxMetricTimestamp(serverID string) (model.Time, bool, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT MAX("Timestamp") FROM "MetricSnapshots" WHERE "ServerId"=?`, serverID).Scan(&raw)
	if err != nil {
		return model.Time{}, false, fmt.Errorf("max metric timestamp: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return model.Time{}, false, nil
	}
	t, err := model.ParseDB(raw.String)
	if err != nil {
		return model.Time{}, false, err
	}
	return t, true, nil
}

// LatestMetric returns the newest sample for a server, or nil if there is none.
func (s *Store) LatestMetric(serverID string) (*model.Metric, error) {
	metrics, err := s.queryMetrics(`
		SELECT "Id","Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds"
		FROM "MetricSnapshots" WHERE "ServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("latest metric %s: %w", serverID, err)
	}
	if len(metrics) == 0 {
		return nil, nil
	}
	return &metrics[0], nil
}

// MetricsSince returns a server's samples from since onwards, oldest first.
func (s *Store) MetricsSince(serverID string, since model.Time) ([]model.Metric, error) {
	metrics, err := s.queryMetrics(`
		SELECT "Id","Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds"
		FROM "MetricSnapshots"
		WHERE "ServerId"=? AND "Timestamp">=?
		ORDER BY "Timestamp"`, serverID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("metrics since: %w", err)
	}
	return metrics, nil
}

// MetricsBucketed returns a server's samples from since onwards, oldest first,
// averaged into buckets of the given width. A bucket of zero returns the raw
// samples. It exists because the wide ranges are otherwise unusable: a week of
// five-second samples is over a hundred thousand points per chart, which the
// browser spends its time drawing and no eye can read.
//
// Readings worth averaging are averaged; the rest come from the newest sample
// in the bucket, which is what the bare "UptimeSeconds" and "Id" columns
// select. SQLite fills a bare column from the row that produced the query's
// single min() or max() aggregate - here MAX("Timestamp"). A second min() or
// max() would leave which row that is undefined, so the uptime has to stay a
// bare column rather than becoming a MAX of its own. The id carries that same
// row's volumes, so the disks reported for a bucket are one real reading
// rather than a blend of drives that were never mounted at the same time.
func (s *Store) MetricsBucketed(serverID string, since model.Time, bucket time.Duration) ([]model.Metric, error) {
	seconds := int64(bucket / time.Second)
	if seconds <= 0 {
		return s.MetricsSince(serverID, since)
	}

	metrics, err := s.queryMetrics(`
		SELECT "Id",MAX("Timestamp"),AVG("CpuPercent"),AVG("CpuTempC"),AVG("MemoryPercent"),
		       AVG("MemoryTotalMb"),AVG("MemoryUsedMb"),"UptimeSeconds"
		FROM "MetricSnapshots"
		WHERE "ServerId"=? AND "Timestamp">=?
		GROUP BY CAST(strftime('%s',"Timestamp") AS INTEGER)/?
		ORDER BY MAX("Timestamp")`, serverID, since.DB(), seconds)
	if err != nil {
		return nil, fmt.Errorf("metrics bucketed: %w", err)
	}
	return metrics, nil
}

// queryMetrics runs a statement selecting the columns scanMetric expects, the
// row id first, and fills in each sample's volumes.
func (s *Store) queryMetrics(query string, args ...any) ([]model.Metric, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []int64{}
	metrics := []model.Metric{}
	for rows.Next() {
		id, m, err := scanMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("scan metric: %w", err)
		}
		ids = append(ids, id)
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachDisks(ids, metrics); err != nil {
		return nil, err
	}
	return metrics, nil
}

// diskBatch caps how many ids go into one IN clause. SQLite's older default
// parameter limit is 999, and a raw hour of five-second samples already spends
// most of that.
const diskBatch = 500

// attachDisks fills in the volumes of each sample, where ids[i] identifies
// metrics[i]. Batched rather than queried per sample: a chart's range is
// hundreds of samples, and asking for each one's disks separately is the usual
// N+1.
func (s *Store) attachDisks(ids []int64, metrics []model.Metric) error {
	byMetric := make(map[int64][]model.Disk, len(ids))

	for start := 0; start < len(ids); start += diskBatch {
		batch := ids[start:min(start+diskBatch, len(ids))]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}

		rows, err := s.db.Query(`
			SELECT "MetricId","Name","TotalGb","UsedGb","FreeGb","UsagePercent","TempC"
			FROM "MetricDisks"
			WHERE "MetricId" IN (`+strings.TrimPrefix(strings.Repeat(",?", len(batch)), ",")+`)
			ORDER BY "Id"`, args...)
		if err != nil {
			return fmt.Errorf("query disks: %w", err)
		}

		for rows.Next() {
			var (
				metricID int64
				disk     model.Disk
			)
			if err := rows.Scan(&metricID, &disk.Name, &disk.TotalGb, &disk.UsedGb,
				&disk.FreeGb, &disk.UsagePercent, &disk.TempC); err != nil {
				rows.Close()
				return fmt.Errorf("scan disk: %w", err)
			}
			byMetric[metricID] = append(byMetric[metricID], disk)
		}
		// Closed here rather than deferred: this runs once per batch, and a
		// deferred close would hold every batch open until the function ended.
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read disks: %w", err)
		}
	}

	for i, id := range ids {
		metrics[i].Disks = byMetric[id]
	}
	return nil
}

func scanMetric(sc scanner) (int64, model.Metric, error) {
	var (
		m  model.Metric
		id int64
		ts string
	)
	if err := sc.Scan(&id, &ts, &m.CpuPercent, &m.CpuTempC, &m.MemoryPercent, &m.MemoryTotalMb,
		&m.MemoryUsedMb, &m.UptimeSeconds); err != nil {
		return 0, model.Metric{}, err
	}
	t, err := model.ParseDB(ts)
	if err != nil {
		return 0, model.Metric{}, err
	}
	m.Timestamp = t
	return id, m, nil
}

// ── Availability ────────────────────────────────────────────────────────────

// AvailabilityRow is one stored reachability observation, including its origin.
type AvailabilityRow struct {
	FromServerID string
	ToServerID   string
	Timestamp    model.Time
	IsAvailable  bool
	LatencyMs    *float64
	HTTPStatus   *int
}

// InsertAvailability appends observations made by fromServerID.
func (s *Store) InsertAvailability(fromServerID string, records []model.Availability) error {
	if len(records) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin availability tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO "AvailabilityRecords"
			("FromServerId","ToServerId","Timestamp","IsAvailable","LatencyMs","HttpStatus")
		VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare availability insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.Exec(fromServerID, r.ToServerID, r.Timestamp.DB(),
			r.IsAvailable, r.LatencyMs, r.HTTPStatus); err != nil {
			return fmt.Errorf("insert availability: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit availability: %w", err)
	}
	return nil
}

// MaxAvailabilityTimestamp returns the newest observation time recorded by a
// given origin server, used to drop duplicates arriving from a peer resync.
func (s *Store) MaxAvailabilityTimestamp(fromServerID string) (model.Time, bool, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT MAX("Timestamp") FROM "AvailabilityRecords" WHERE "FromServerId"=?`, fromServerID).Scan(&raw)
	if err != nil {
		return model.Time{}, false, fmt.Errorf("max availability timestamp: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return model.Time{}, false, nil
	}
	t, err := model.ParseDB(raw.String)
	if err != nil {
		return model.Time{}, false, err
	}
	return t, true, nil
}

// AvailabilitySince returns every observation newer than since, from any origin.
func (s *Store) AvailabilitySince(since model.Time) ([]AvailabilityRow, error) {
	return s.queryAvailability(`
		SELECT "FromServerId","ToServerId","Timestamp","IsAvailable","LatencyMs","HttpStatus"
		FROM "AvailabilityRecords"
		WHERE "Timestamp">=?
		ORDER BY "Timestamp"`, since.DB())
}

// LatestAvailability returns the most recent observation on one edge of the
// mesh, or nil when that pair has never been measured.
func (s *Store) LatestAvailability(fromServerID, toServerID string) (*AvailabilityRow, error) {
	rows, err := s.queryAvailability(`
		SELECT "FromServerId","ToServerId","Timestamp","IsAvailable","LatencyMs","HttpStatus"
		FROM "AvailabilityRecords"
		WHERE "FromServerId"=? AND "ToServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, fromServerID, toServerID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// AvailabilityHistory returns one edge's observations from since onwards.
func (s *Store) AvailabilityHistory(fromServerID, toServerID string, since model.Time) ([]AvailabilityRow, error) {
	return s.queryAvailability(`
		SELECT "FromServerId","ToServerId","Timestamp","IsAvailable","LatencyMs","HttpStatus"
		FROM "AvailabilityRecords"
		WHERE "FromServerId"=? AND "ToServerId"=? AND "Timestamp">=?
		ORDER BY "Timestamp"`, fromServerID, toServerID, since.DB())
}

func (s *Store) queryAvailability(query string, args ...any) ([]AvailabilityRow, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query availability: %w", err)
	}
	defer rows.Close()

	out := []AvailabilityRow{}
	for rows.Next() {
		var (
			r  AvailabilityRow
			ts string
		)
		if err := rows.Scan(&r.FromServerID, &r.ToServerID, &ts, &r.IsAvailable, &r.LatencyMs, &r.HTTPStatus); err != nil {
			return nil, fmt.Errorf("scan availability: %w", err)
		}
		t, err := model.ParseDB(ts)
		if err != nil {
			return nil, err
		}
		r.Timestamp = t
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Retention ───────────────────────────────────────────────────────────────

// Prune deletes metric and availability rows older than cutoff.
func (s *Store) Prune(cutoff model.Time) error {
	// Before their snapshots: once the rows identifying them are gone, the
	// volumes cannot be found to delete.
	if _, err := s.db.Exec(`
		DELETE FROM "MetricDisks"
		WHERE "MetricId" IN (SELECT "Id" FROM "MetricSnapshots" WHERE "Timestamp"<?)`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune disks: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM "MetricSnapshots" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune metrics: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM "AvailabilityRecords" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune availability: %w", err)
	}
	return nil
}
