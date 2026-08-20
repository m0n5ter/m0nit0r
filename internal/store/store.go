// Package store persists servers, metric samples and availability records in
// SQLite. The schema and the TEXT timestamp encoding match what the previous
// EF Core implementation created, so an existing monitor.db opens unchanged.
package store

import (
	"database/sql"
	"errors"
	"fmt"

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
    "UptimeSeconds" REAL NOT NULL,
    "DisksJson"     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS "IX_MetricSnapshots_ServerId_Timestamp"
    ON "MetricSnapshots" ("ServerId", "Timestamp");

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

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// migrations are columns added after the schema above was first shipped. A
// CREATE TABLE IF NOT EXISTS leaves an existing table exactly as it is, so a
// database created by an earlier build needs them added explicitly. Each is
// nullable, which is what lets rows recorded before the column existed stay
// distinguishable from rows whose sensor reported nothing.
var migrations = []struct{ table, column, ddl string }{
	{"MetricSnapshots", "CpuTempC", `ALTER TABLE "MetricSnapshots" ADD COLUMN "CpuTempC" REAL NULL`},
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		present, err := hasColumn(db, m.table, m.column)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

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

// InsertMetrics appends samples for serverID and reports how many were written.
func (s *Store) InsertMetrics(serverID string, metrics []model.Metric) (int, error) {
	if len(metrics) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin metrics tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO "MetricSnapshots"
			("ServerId","Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds","DisksJson")
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare metric insert: %w", err)
	}
	defer stmt.Close()

	for _, m := range metrics {
		disks := m.DisksJSON
		if disks == "" {
			disks = "[]"
		}
		if _, err := stmt.Exec(serverID, m.Timestamp.DB(), m.CpuPercent, m.CpuTempC, m.MemoryPercent,
			m.MemoryTotalMb, m.MemoryUsedMb, m.UptimeSeconds, disks); err != nil {
			return 0, fmt.Errorf("insert metric: %w", err)
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
	row := s.db.QueryRow(`
		SELECT "Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds","DisksJson"
		FROM "MetricSnapshots" WHERE "ServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, serverID)

	m, err := scanMetric(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest metric %s: %w", serverID, err)
	}
	return &m, nil
}

// MetricsSince returns a server's samples from since onwards, oldest first.
func (s *Store) MetricsSince(serverID string, since model.Time) ([]model.Metric, error) {
	rows, err := s.db.Query(`
		SELECT "Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds","DisksJson"
		FROM "MetricSnapshots"
		WHERE "ServerId"=? AND "Timestamp">=?
		ORDER BY "Timestamp"`, serverID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("metrics since: %w", err)
	}
	defer rows.Close()

	metrics := []model.Metric{}
	for rows.Next() {
		m, err := scanMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("scan metric: %w", err)
		}
		metrics = append(metrics, m)
	}
	return metrics, rows.Err()
}

func scanMetric(sc scanner) (model.Metric, error) {
	var (
		m  model.Metric
		ts string
	)
	if err := sc.Scan(&ts, &m.CpuPercent, &m.CpuTempC, &m.MemoryPercent, &m.MemoryTotalMb,
		&m.MemoryUsedMb, &m.UptimeSeconds, &m.DisksJSON); err != nil {
		return model.Metric{}, err
	}
	t, err := model.ParseDB(ts)
	if err != nil {
		return model.Metric{}, err
	}
	m.Timestamp = t
	return m, nil
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
	if _, err := s.db.Exec(`DELETE FROM "MetricSnapshots" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune metrics: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM "AvailabilityRecords" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune availability: %w", err)
	}
	return nil
}
