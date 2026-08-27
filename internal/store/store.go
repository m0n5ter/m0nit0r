// Package store persists servers, metric samples and availability records in
// SQLite.
//
// Servers are referenced by an integer row id. The identity a node publishes -
// the GUID in server-id.txt - lives in "Servers"."Uid" and is what crosses the
// wire, but repeating it in every one of the millions of metric and
// availability rows cost more space than the readings themselves and turned
// every lookup into a 36-byte string comparison. Timestamps are stored the same
// way, as integer Unix milliseconds rather than padded ISO text.
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
    "Id"       INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "Uid"      TEXT NOT NULL UNIQUE,
    "Name"     TEXT NOT NULL,
    "Location" TEXT NOT NULL,
    "Url"      TEXT NULL,
    "IsSelf"   INTEGER NOT NULL,
    "Alerts"   INTEGER NOT NULL DEFAULT 0,
    "LastSeen" INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS "MetricSnapshots" (
    "Id"            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "ServerId"      INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "Timestamp"     INTEGER NOT NULL,
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
    "FromServerId" INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "ToServerId"   INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "Timestamp"    INTEGER NOT NULL,
    "IsAvailable"  INTEGER NOT NULL,
    "LatencyMs"    REAL NULL,
    "HttpStatus"   INTEGER NULL
);

CREATE INDEX IF NOT EXISTS "IX_AvailabilityRecords_FromServerId_ToServerId_Timestamp"
    ON "AvailabilityRecords" ("FromServerId", "ToServerId", "Timestamp");

-- One row per alert a node actually sent out, replicated across the mesh the
-- same way observations are. Several nodes may be configured to raise alerts,
-- and they all watch the same set of peers, so without a record of what has
-- already been announced one outage would arrive as one message per alerting
-- node. A node reads this table to find out whether somebody has already said
-- what it was about to say.
--
-- Deliberately not keyed by outage: two nodes notice the same node failing a
-- few seconds apart and would compute different start times for it. "The most
-- recent thing anyone announced about this node" needs no such agreement.
CREATE TABLE IF NOT EXISTS "AlertNotices" (
    "Id"           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "FromServerId" INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "ToServerId"   INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "Timestamp"    INTEGER NOT NULL,
    "IsDown"       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS "IX_AlertNotices_ToServerId_Timestamp"
    ON "AlertNotices" ("ToServerId", "Timestamp");

CREATE INDEX IF NOT EXISTS "IX_AlertNotices_FromServerId_Timestamp"
    ON "AlertNotices" ("FromServerId", "Timestamp");
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
	// There is deliberately no migration off that one: the disks of every
	// stored sample are in a format this build cannot read back, and retention
	// keeps a week at most, so the history is cheap to lose and not worth the
	// code to carry across. Dropping the tables unasked would be worse than
	// refusing, though, so this says what it found and leaves the file for its
	// owner to remove.
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

	// The move to integer keys and timestamps, unlike the disk split, loses
	// nothing: a GUID maps to a row id and an ISO string to a millisecond
	// count, both exactly. So this one is migrated rather than refused - the
	// history is replaceable, but the peer register is what holds the mesh
	// together, and rebuilding that is an operator's afternoon.
	migrate, err := needsIntegerKeys(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if migrate {
		if err := toIntegerKeys(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate %s to integer keys: %w", path, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	// Columns added to a table that already exists, which CREATE TABLE IF NOT
	// EXISTS above cannot reach. Run after the schema so the table is there to
	// alter on a database being created from nothing.
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, err
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

// UpsertSelf writes this instance's own row and returns its id. A blank url
// leaves any stored address alone, the same rule UpsertPeer follows: a node
// started without a PublicUrl configured has no better address to offer than
// the one it already published.
func (s *Store) UpsertSelf(uid, name, location, url string, alerts bool) (int64, error) {
	id, err := s.upsertServer(uid, name, location, url, true, alerts, model.Now())
	if err != nil {
		return 0, fmt.Errorf("upsert self: %w", err)
	}
	return id, nil
}

// UpsertPeer records a peer's identity and returns its id. A blank url leaves
// any stored URL alone, so an introduction that omits it cannot erase a working
// address.
func (s *Store) UpsertPeer(uid, name, location, url string, alerts bool, lastSeen model.Time) (int64, error) {
	id, err := s.upsertServer(uid, name, location, url, false, alerts, lastSeen)
	if err != nil {
		return 0, fmt.Errorf("upsert peer %s: %w", uid, err)
	}
	return id, nil
}

// upsertServer writes a server row keyed by its uid and returns the row id.
// IsSelf is only ever raised, never cleared: an introduction arriving with this
// node's own id would otherwise demote it to a peer of itself.
// Alerts, unlike IsSelf, is overwritten: it is the node's current configuration
// and a node that has had its Telegram credentials removed has to stop counting
// as one of the alerting nodes, or the rest of the mesh keeps deferring to
// announcements it will never make.
func (s *Store) upsertServer(uid, name, location, url string, self, alerts bool, lastSeen model.Time) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO "Servers" ("Uid","Name","Location","Url","IsSelf","Alerts","LastSeen")
		VALUES (?,?,?,NULLIF(?,''),?,?,?)
		ON CONFLICT("Uid") DO UPDATE SET
			"Name"=excluded."Name",
			"Location"=excluded."Location",
			"Url"=COALESCE(excluded."Url", "Servers"."Url"),
			"IsSelf"=MAX("Servers"."IsSelf", excluded."IsSelf"),
			"Alerts"=excluded."Alerts",
			"LastSeen"=excluded."LastSeen"
		RETURNING "Id"`,
		uid, name, location, url, self, alerts, lastSeen.DB()).Scan(&id)
	return id, err
}

// EnsureServer returns the row id for a uid, registering the server if this
// node has never heard of it.
//
// A peer forwards the reachability it observed towards every node it knows, and
// a cross-introduction that failed can leave this node holding an observation
// about a third party it has no row for. Recording it under a placeholder keeps
// the reading rather than discarding it; the name arrives with the next
// introduction and overwrites the id standing in for it.
func (s *Store) EnsureServer(uid string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT "Id" FROM "Servers" WHERE "Uid"=?`, uid).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("resolve server %s: %w", uid, err)
	}

	// The name stands in for itself until an introduction supplies a real one,
	// and the unset LastSeen is what the dashboard reads as "no data" rather
	// than as a node that has gone quiet. The conflict clause updates the uid to
	// the value it already holds: a no-op that exists only so RETURNING still
	// yields the row if one appeared since the select above.
	err = s.db.QueryRow(`
		INSERT INTO "Servers" ("Uid","Name","Location","Url","IsSelf","Alerts","LastSeen")
		VALUES (?,?,'',NULL,0,0,?)
		ON CONFLICT("Uid") DO UPDATE SET "Uid"=excluded."Uid"
		RETURNING "Id"`, uid, uid, model.Time{}.DB()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("register server %s: %w", uid, err)
	}
	return id, nil
}

// SetPeerURL overwrites a peer's address unconditionally.
func (s *Store) SetPeerURL(id int64, url string) error {
	_, err := s.db.Exec(`UPDATE "Servers" SET "Url"=NULLIF(?,'') WHERE "Id"=?`, url, id)
	if err != nil {
		return fmt.Errorf("set peer url %d: %w", id, err)
	}
	return nil
}

// TouchLastSeen records that a server was reachable at t.
func (s *Store) TouchLastSeen(id int64, t model.Time) error {
	_, err := s.db.Exec(`UPDATE "Servers" SET "LastSeen"=? WHERE "Id"=?`, t.DB(), id)
	if err != nil {
		return fmt.Errorf("touch last seen %d: %w", id, err)
	}
	return nil
}

const selectServer = `SELECT "Id","Uid","Name","Location",COALESCE("Url",''),"IsSelf","Alerts","LastSeen" FROM "Servers"`

// ServerByUID returns the server published under uid. The bool reports whether
// it existed, which is how the dashboard and peer endpoints turn the identity
// they were handed into the id the tables are keyed by.
func (s *Store) ServerByUID(uid string) (model.Server, bool, error) {
	srv, err := scanServer(s.db.QueryRow(selectServer+` WHERE "Uid"=?`, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Server{}, false, nil
	}
	if err != nil {
		return model.Server{}, false, fmt.Errorf("get server %s: %w", uid, err)
	}
	return srv, true, nil
}

// ListServers returns every known server, self included.
func (s *Store) ListServers() ([]model.Server, error) {
	return s.queryServers(selectServer + ` ORDER BY "IsSelf" DESC, "Name"`)
}

// ListPeers returns non-self servers that still have an address configured.
func (s *Store) ListPeers() ([]model.Server, error) {
	return s.queryServers(selectServer + ` WHERE "IsSelf"=0 AND "Url" IS NOT NULL AND "Url"<>'' ORDER BY "Name"`)
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
		lastSeen int64
	)
	if err := sc.Scan(&srv.ID, &srv.UID, &srv.Name, &srv.Location, &srv.URL, &srv.IsSelf,
		&srv.Alerts, &lastSeen); err != nil {
		return model.Server{}, err
	}
	srv.LastSeen = model.FromDB(lastSeen)
	return srv, nil
}

// ── Metrics ─────────────────────────────────────────────────────────────────

// InsertMetrics appends samples for serverID and reports how many were
// written. A sample's volumes go in the same transaction, so a snapshot never
// exists without the disks it reported.
func (s *Store) InsertMetrics(serverID int64, metrics []model.Metric) (int, error) {
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
func (s *Store) MaxMetricTimestamp(serverID int64) (model.Time, bool, error) {
	return s.maxTimestamp(`SELECT MAX("Timestamp") FROM "MetricSnapshots" WHERE "ServerId"=?`,
		"max metric timestamp", serverID)
}

// LatestMetric returns the newest sample for a server, or nil if there is none.
func (s *Store) LatestMetric(serverID int64) (*model.Metric, error) {
	metrics, err := s.queryMetrics(`
		SELECT "Id","Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds"
		FROM "MetricSnapshots" WHERE "ServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("latest metric %d: %w", serverID, err)
	}
	if len(metrics) == 0 {
		return nil, nil
	}
	return &metrics[0], nil
}

// MetricsSince returns a server's samples from since onwards, oldest first.
func (s *Store) MetricsSince(serverID int64, since model.Time) ([]model.Metric, error) {
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
//
// Grouping is integer division on the stored millisecond count. It used to go
// through strftime, which had to parse a date string per row before it could
// divide anything.
func (s *Store) MetricsBucketed(serverID int64, since model.Time, bucket time.Duration) ([]model.Metric, error) {
	width := bucket.Milliseconds()
	if width <= 0 {
		return s.MetricsSince(serverID, since)
	}

	metrics, err := s.queryMetrics(`
		SELECT "Id",MAX("Timestamp"),AVG("CpuPercent"),AVG("CpuTempC"),AVG("MemoryPercent"),
		       AVG("MemoryTotalMb"),AVG("MemoryUsedMb"),"UptimeSeconds"
		FROM "MetricSnapshots"
		WHERE "ServerId"=? AND "Timestamp">=?
		GROUP BY "Timestamp"/?
		ORDER BY MAX("Timestamp")`, serverID, since.DB(), width)
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
		ts int64
	)
	if err := sc.Scan(&id, &ts, &m.CpuPercent, &m.CpuTempC, &m.MemoryPercent, &m.MemoryTotalMb,
		&m.MemoryUsedMb, &m.UptimeSeconds); err != nil {
		return 0, model.Metric{}, err
	}
	m.Timestamp = model.FromDB(ts)
	return id, m, nil
}

// ── Availability ────────────────────────────────────────────────────────────

// Observation is one reachability reading about a server in this database,
// named by its row id. Everything written is in this form, and so is one edge's
// history, where the caller already knows both ends.
type Observation struct {
	ToServerID  int64
	Timestamp   model.Time
	IsAvailable bool
	LatencyMs   *float64
	HTTPStatus  *int
}

// AvailabilityRow is one stored observation with both ends resolved to the ids
// their nodes publish, which is what the dashboard's matrix is drawn from - it
// spans the whole mesh, so neither end is known in advance.
type AvailabilityRow struct {
	FromUID     string
	ToUID       string
	Timestamp   model.Time
	IsAvailable bool
	LatencyMs   *float64
	HTTPStatus  *int
}

// InsertAvailability appends observations made by fromServerID.
func (s *Store) InsertAvailability(fromServerID int64, records []Observation) error {
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
func (s *Store) MaxAvailabilityTimestamp(fromServerID int64) (model.Time, bool, error) {
	return s.maxTimestamp(`SELECT MAX("Timestamp") FROM "AvailabilityRecords" WHERE "FromServerId"=?`,
		"max availability timestamp", fromServerID)
}

// maxTimestamp runs a MAX("Timestamp") query, reporting through the bool
// whether there was any row to take a maximum of.
func (s *Store) maxTimestamp(query, what string, args ...any) (model.Time, bool, error) {
	var raw sql.NullInt64
	if err := s.db.QueryRow(query, args...).Scan(&raw); err != nil {
		return model.Time{}, false, fmt.Errorf("%s: %w", what, err)
	}
	if !raw.Valid {
		return model.Time{}, false, nil
	}
	return model.FromDB(raw.Int64), true, nil
}

// AvailabilitySince returns every observation newer than since, from any
// origin, for the dashboard's matrix.
func (s *Store) AvailabilitySince(since model.Time) ([]AvailabilityRow, error) {
	rows, err := s.db.Query(`
		SELECT f."Uid",t."Uid",a."Timestamp",a."IsAvailable",a."LatencyMs",a."HttpStatus"
		FROM "AvailabilityRecords" a
		JOIN "Servers" f ON f."Id"=a."FromServerId"
		JOIN "Servers" t ON t."Id"=a."ToServerId"
		WHERE a."Timestamp">=?
		ORDER BY a."Timestamp"`, since.DB())
	if err != nil {
		return nil, fmt.Errorf("availability since: %w", err)
	}
	defer rows.Close()

	out := []AvailabilityRow{}
	for rows.Next() {
		var (
			r  AvailabilityRow
			ts int64
		)
		if err := rows.Scan(&r.FromUID, &r.ToUID, &ts, &r.IsAvailable, &r.LatencyMs, &r.HTTPStatus); err != nil {
			return nil, fmt.Errorf("scan availability: %w", err)
		}
		r.Timestamp = model.FromDB(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OwnAvailabilitySince returns the observations fromServerID itself made from
// since onwards, in the form a peer is sent them.
//
// Only a node's own readings are its to forward - relaying what a peer reported
// would duplicate it around the mesh - so the origin is a condition on the
// index rather than a filter applied to every row afterwards.
func (s *Store) OwnAvailabilitySince(fromServerID int64, since model.Time) ([]model.Availability, error) {
	rows, err := s.db.Query(`
		SELECT t."Uid",a."Timestamp",a."IsAvailable",a."LatencyMs",a."HttpStatus"
		FROM "AvailabilityRecords" a
		JOIN "Servers" t ON t."Id"=a."ToServerId"
		WHERE a."FromServerId"=? AND a."Timestamp">=?
		ORDER BY a."Timestamp"`, fromServerID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("own availability since: %w", err)
	}
	defer rows.Close()

	out := []model.Availability{}
	for rows.Next() {
		var (
			a  model.Availability
			ts int64
		)
		if err := rows.Scan(&a.ToServerID, &ts, &a.IsAvailable, &a.LatencyMs, &a.HTTPStatus); err != nil {
			return nil, fmt.Errorf("scan own availability: %w", err)
		}
		a.Timestamp = model.FromDB(ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

// LatestAvailability reports the most recent observation on one edge of the
// mesh. The second bool is false when that pair has never been measured, which
// is not the same as having been measured as down.
func (s *Store) LatestAvailability(fromServerID, toServerID int64) (available, found bool, err error) {
	err = s.db.QueryRow(`
		SELECT "IsAvailable" FROM "AvailabilityRecords"
		WHERE "FromServerId"=? AND "ToServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, fromServerID, toServerID).Scan(&available)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("latest availability %d->%d: %w", fromServerID, toServerID, err)
	}
	return available, true, nil
}

// Streak reports how one edge of the mesh currently stands and when it came to
// stand that way: available is the state of the newest observation, and since
// is the timestamp of the first observation in the unbroken run of that state.
// found is false when the pair has never been measured.
//
// A node that has been unreachable for longer than the retention window has no
// surviving observation of it being up, so the run appears to begin at the
// oldest record still held. The reported duration is then a floor rather than
// the true one, which for an outage already a week old is a distinction without
// a difference.
func (s *Store) Streak(fromServerID, toServerID int64) (available bool, since model.Time, found bool, err error) {
	var newest int64
	err = s.db.QueryRow(`
		SELECT "IsAvailable","Timestamp" FROM "AvailabilityRecords"
		WHERE "FromServerId"=? AND "ToServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, fromServerID, toServerID).Scan(&available, &newest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, model.Time{}, false, nil
	}
	if err != nil {
		return false, model.Time{}, false, fmt.Errorf("streak %d->%d: %w", fromServerID, toServerID, err)
	}

	// The run began at the oldest observation newer than the last one that
	// disagreed with it. The subquery is over the same index as the outer
	// query, which is what keeps this two seeks rather than a scan.
	var start sql.NullInt64
	err = s.db.QueryRow(`
		SELECT MIN("Timestamp") FROM "AvailabilityRecords"
		WHERE "FromServerId"=? AND "ToServerId"=? AND "IsAvailable"=?
		  AND "Timestamp" > COALESCE((
		      SELECT MAX("Timestamp") FROM "AvailabilityRecords"
		      WHERE "FromServerId"=? AND "ToServerId"=? AND "IsAvailable"<>?), -1)`,
		fromServerID, toServerID, available, fromServerID, toServerID, available).Scan(&start)
	if err != nil {
		return false, model.Time{}, false, fmt.Errorf("streak start %d->%d: %w", fromServerID, toServerID, err)
	}
	if !start.Valid {
		// Cannot happen while the row read above is still there, but a
		// concurrent prune is cheaper to tolerate than to exclude.
		return available, model.FromDB(newest), true, nil
	}
	return available, model.FromDB(start.Int64), true, nil
}

// AvailabilityHistory returns one edge's observations from since onwards.
func (s *Store) AvailabilityHistory(fromServerID, toServerID int64, since model.Time) ([]Observation, error) {
	rows, err := s.db.Query(`
		SELECT "Timestamp","IsAvailable","LatencyMs","HttpStatus"
		FROM "AvailabilityRecords"
		WHERE "FromServerId"=? AND "ToServerId"=? AND "Timestamp">=?
		ORDER BY "Timestamp"`, fromServerID, toServerID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("availability history: %w", err)
	}
	defer rows.Close()

	out := []Observation{}
	for rows.Next() {
		o := Observation{ToServerID: toServerID}
		var ts int64
		if err := rows.Scan(&ts, &o.IsAvailable, &o.LatencyMs, &o.HTTPStatus); err != nil {
			return nil, fmt.Errorf("scan availability history: %w", err)
		}
		o.Timestamp = model.FromDB(ts)
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── Alert notices ───────────────────────────────────────────────────────────

// InsertNotices records that fromServerID announced these state changes.
func (s *Store) InsertNotices(fromServerID int64, notices []Notice) error {
	if len(notices) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin notices tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO "AlertNotices" ("FromServerId","ToServerId","Timestamp","IsDown")
		VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare notice insert: %w", err)
	}
	defer stmt.Close()

	for _, n := range notices {
		if _, err := stmt.Exec(fromServerID, n.ToServerID, n.Timestamp.DB(), n.IsDown); err != nil {
			return fmt.Errorf("insert notice: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit notices: %w", err)
	}
	return nil
}

// Notice is one announcement about a server in this database, named by its row
// id. This is the form notices are written and read back in locally, the same
// way Observation is for reachability.
type Notice struct {
	ToServerID int64
	Timestamp  model.Time
	IsDown     bool
}

// LatestNotice returns the most recent announcement anyone in the mesh made
// about a server, whoever made it. The bool is false when nothing has ever been
// announced about it, which is what distinguishes a node nobody has complained
// about from one that was already reported down.
func (s *Store) LatestNotice(toServerID int64) (Notice, bool, error) {
	n := Notice{ToServerID: toServerID}
	var ts int64
	err := s.db.QueryRow(`
		SELECT "Timestamp","IsDown" FROM "AlertNotices"
		WHERE "ToServerId"=?
		ORDER BY "Timestamp" DESC LIMIT 1`, toServerID).Scan(&ts, &n.IsDown)
	if errors.Is(err, sql.ErrNoRows) {
		return Notice{}, false, nil
	}
	if err != nil {
		return Notice{}, false, fmt.Errorf("latest notice %d: %w", toServerID, err)
	}
	n.Timestamp = model.FromDB(ts)
	return n, true, nil
}

// MaxNoticeTimestamp returns the newest announcement time recorded for an
// origin, used to drop the duplicates a peer resync brings back.
func (s *Store) MaxNoticeTimestamp(fromServerID int64) (model.Time, bool, error) {
	return s.maxTimestamp(`SELECT MAX("Timestamp") FROM "AlertNotices" WHERE "FromServerId"=?`,
		"max notice timestamp", fromServerID)
}

// OwnNoticesSince returns the announcements fromServerID itself made from since
// onwards, in the form a peer is sent them. As with observations, only a node's
// own are its to forward.
func (s *Store) OwnNoticesSince(fromServerID int64, since model.Time) ([]model.Notice, error) {
	rows, err := s.db.Query(`
		SELECT t."Uid",n."Timestamp",n."IsDown"
		FROM "AlertNotices" n
		JOIN "Servers" t ON t."Id"=n."ToServerId"
		WHERE n."FromServerId"=? AND n."Timestamp">=?
		ORDER BY n."Timestamp"`, fromServerID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("own notices since: %w", err)
	}
	defer rows.Close()

	out := []model.Notice{}
	for rows.Next() {
		var (
			n  model.Notice
			ts int64
		)
		if err := rows.Scan(&n.ToServerID, &ts, &n.IsDown); err != nil {
			return nil, fmt.Errorf("scan own notice: %w", err)
		}
		n.Timestamp = model.FromDB(ts)
		out = append(out, n)
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
	if _, err := s.db.Exec(`DELETE FROM "AlertNotices" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune alert notices: %w", err)
	}
	return nil
}
