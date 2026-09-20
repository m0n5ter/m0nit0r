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
    "PushOnly" INTEGER NOT NULL DEFAULT 0,
    -- Whether the operator took this node out of the mesh, and when that was
    -- decided. The row outlives the membership: it is the tombstone that keeps
    -- a removed node from being learned back from a neighbour or registering
    -- itself again by pushing here, and MemberAt is what orders one decision
    -- against another when they meet. A zero MemberAt means no decision has
    -- ever been announced about this node, so nothing about it is published to
    -- the mesh - which is also how a removal inherited from a database written
    -- before this column existed stays local to the node that made it.
    "Removed"  INTEGER NOT NULL DEFAULT 0,
    "MemberAt" INTEGER NOT NULL DEFAULT 0,
    "LastSeen" INTEGER NOT NULL
);

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

-- ── The series store ────────────────────────────────────────────────────────
--
-- Everything measured, in one shape: a parameter, up to two dimensions, a
-- time, and a number. The three tables above record the same readings in the
-- shape the first build stored them, and are kept in step until every node in
-- a mesh is new enough to have stopped needing them.

-- A drive's name is a string, and putting it in the key of every row that
-- mentions it would cost more than the reading. Here it becomes a small
-- integer, once.
CREATE TABLE IF NOT EXISTS "Volumes" (
    "Id"       INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    "ServerId" INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "Name"     TEXT NOT NULL,
    UNIQUE ("ServerId","Name")
);

-- Raw readings, kept for twenty minutes. Separate from the aggregates because
-- they churn: written every few seconds and deleted just as fast, where the
-- aggregates are written once and read for a year. In one B-tree that delete
-- traffic would fragment the pages holding the daily rows.
--
-- Value is the reading multiplied by the parameter's scale, because SQLite
-- spends eight bytes on every REAL and as few as one on an integer. It is NULL
-- when the reading was attempted and produced nothing - a host with no
-- temperature sensor, a probe that timed out - which is a different thing from
-- the row being absent, and that difference is what the counters downstream
-- are built from.
--
-- Server2 names the node observed, for edge parameters; Volume names the
-- drive, for disk parameters; both are 0 otherwise. They are never both used,
-- but they are kept apart rather than overloaded into one column: a column
-- whose meaning depends on another column is a query waiting to join against
-- the wrong table.
CREATE TABLE IF NOT EXISTS "Samples" (
    "Param"     INTEGER NOT NULL,
    "Server1"   INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "Server2"   INTEGER NOT NULL,
    "Volume"    INTEGER NOT NULL,
    "Timestamp" INTEGER NOT NULL,
    "Value"     INTEGER NULL,
    "IsOk"      INTEGER NOT NULL,
    PRIMARY KEY ("Server1","Param","Server2","Volume","Timestamp")
) WITHOUT ROWID;

-- The matrix asks for every route at once over the last couple of minutes,
-- which the key above cannot serve: it leads with the node doing the
-- observing, so the time it selects on is its last column and the whole table
-- has to be walked. This orders the same rows by time.
--
-- Partial, over the one parameter drawn that way. The readings about a machine
-- are only ever asked for one machine at a time, which the key already
-- answers, and they carry no entry here.
CREATE INDEX IF NOT EXISTS "IX_Samples_Latency_Timestamp"
    ON "Samples" ("Timestamp") WHERE "Param"=9;

-- The aggregation ladder, every level in one table.
--
-- No row id: the key is natural, so WITHOUT ROWID makes the table itself the
-- B-tree over it. The column order is the one a chart asks in - a node, one
-- parameter, a range of buckets, one contiguous walk - which is why Server1
-- leads and Bucket trails. A key led by Param instead would put every node's
-- readings for one parameter together, which is an order nothing asks for.
--
-- Sum rather than an average. Rolling thirty seconds into five minutes is then
-- addition, exact and unweighted, and two views of the same bucket combine by
-- adding; an average would have to be weighted by hand at every rung and would
-- drift a little at each one.
--
-- Three counters, because there are three questions and one flag cannot answer
-- them. Count is how many readings were attempted, and a bucket with fewer
-- than expected has a gap in it. CountOk is how many succeeded, and for an
-- edge CountOk/Count is exactly its availability. CountVal is how many
-- produced a number, which is the only correct divisor for Sum - a probe that
-- timed out records a latency that must not reach the average, and a push-only
-- peer's successful check records no latency at all.
CREATE TABLE IF NOT EXISTS "Rollups" (
    "Param"    INTEGER NOT NULL,
    "Server1"  INTEGER NOT NULL REFERENCES "Servers"("Id"),
    "Server2"  INTEGER NOT NULL,
    "Volume"   INTEGER NOT NULL,
    "Level"    INTEGER NOT NULL,
    "Bucket"   INTEGER NOT NULL,
    "Mn"       INTEGER NULL,
    "Mx"       INTEGER NULL,
    "Sum"      INTEGER NOT NULL,
    "Count"    INTEGER NOT NULL,
    "CountOk"  INTEGER NOT NULL,
    "CountVal" INTEGER NOT NULL,
    PRIMARY KEY ("Server1","Param","Server2","Volume","Level","Bucket")
) WITHOUT ROWID;

-- The matrix asks the opposite way round from a chart: every edge at once, for
-- one recent window. Under the primary key that is a seek per edge, because
-- the time it selects on is the last column. This orders the same rows by time
-- instead.
--
-- Partial, and only over the finest level, which is what makes it affordable:
-- the coarser levels are never asked for this way and carry no entry at all.
-- It is also what retention deletes the finest level through. Every coarser
-- level holds a tenth of the rows or fewer and is swept on a slower schedule,
-- where a scan costs less than an index would have cost all along.
CREATE INDEX IF NOT EXISTS "IX_Rollups_L0_Bucket"
    ON "Rollups" ("Bucket") WHERE "Level"=0;

-- Watermarks and other single facts about this database, so that the roll-up
-- can resume where it stopped rather than rescanning its retention window on
-- every start.
CREATE TABLE IF NOT EXISTS "Meta" (
    "Key"   TEXT NOT NULL PRIMARY KEY,
    "Value" INTEGER NOT NULL
) WITHOUT ROWID;
`

// compatSchema is the three tables the first build stored readings in.
//
// Apart from the rest of the schema because they are the only ones this build
// may drop, and CREATE TABLE IF NOT EXISTS would put them back on the next
// start. Open runs this only while they are still wanted - see RetireLegacy
// for what "wanted" means and how long it lasts.
const compatSchema = `
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

`

// Store owns the database handle.
type Store struct {
	db *sql.DB
	volumeCache
	legacyState
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

	s := &Store{db: db}

	// When this database first had a series store. Written once and never
	// again: it is what the grace before the original tables are dropped is
	// measured from, so an upgraded node starts its window at the upgrade.
	_, dated, err := s.MetaGet(seriesSince)
	if err != nil {
		db.Close()
		return nil, err
	}

	// Retired, rather than merely absent: a database that has been through
	// RetireLegacy carries the stamp above but not the tables, where one being
	// created from nothing carries neither. Only the first must not have them
	// built again, which is why they are not in the schema run above.
	present, err := hasTable(db, "MetricSnapshots")
	if err != nil {
		db.Close()
		return nil, err
	}
	if !(dated && !present) {
		if _, err := db.Exec(compatSchema); err != nil {
			db.Close()
			return nil, fmt.Errorf("create compatibility schema: %w", err)
		}
		s.legacy = true
	}

	if !dated {
		if err := s.MetaSet(seriesSince, model.Now().DB()); err != nil {
			db.Close()
			return nil, err
		}
	}

	return s, nil
}

// hasTable reports whether a table exists, which is how the tables this build
// may have dropped are told from the ones it always creates.
func hasTable(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM "sqlite_master" WHERE "type"='table' AND "name"=?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("inspect schema for %s: %w", name, err)
	}
	return n > 0, nil
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

// SetPushOnly records whether a server is one that only ever pushes. Marking it
// also drops any address it had: a node that has gone push-only no longer
// answers at the one it used to publish, and leaving it in place would keep
// every sync round pushing to it and recording the failure. The update is
// skipped when nothing changes, since every sync carries the flag.
func (s *Store) SetPushOnly(id int64, pushOnly bool) error {
	_, err := s.db.Exec(`
		UPDATE "Servers" SET
			"PushOnly"=?1,
			"Url"=CASE WHEN ?1 THEN NULL ELSE "Url" END
		WHERE "Id"=?2 AND ("PushOnly"<>?1 OR (?1 AND "Url" IS NOT NULL))`, pushOnly, id)
	if err != nil {
		return fmt.Errorf("set push-only %d: %w", id, err)
	}
	return nil
}

// AddPeerIfUnknown registers a peer that some other node named, and reports
// whether it was new. A server this node already syncs with is left exactly as
// it is, and one it removed from the mesh is not brought back by hearing about
// it second-hand: the tombstone is what a neighbour's introduction cannot
// overrule, and readmitting the node is what lifts it.
//
// What the introduction is allowed to complete is a row with no address: either
// one EnsureServer put there - an id seen in somebody's reachability report,
// standing in for its own name - or one left behind by a readmission, which
// clears the tombstone but cannot know the address the node publishes. Both are
// nodes this one simply has not been introduced to yet, and completing them is
// what turns the bare ids a new neighbour brings with it into nodes this one
// syncs with.
func (s *Store) AddPeerIfUnknown(uid, name, location, url string, alerts bool) (bool, error) {
	res, err := s.db.Exec(`
		INSERT INTO "Servers" ("Uid","Name","Location","Url","IsSelf","Alerts","LastSeen")
		VALUES (?1,?2,?3,NULLIF(?4,''),0,?5,?6)
		ON CONFLICT("Uid") DO UPDATE SET
			"Name"=excluded."Name",
			"Location"=excluded."Location",
			"Url"=excluded."Url",
			"Alerts"=excluded."Alerts"
		WHERE "Servers"."IsSelf"=0
			AND "Servers"."Removed"=0
			AND "Servers"."PushOnly"=0
			AND "Servers"."Url" IS NULL`,
		uid, name, location, url, alerts, model.Time{}.DB())
	if err != nil {
		return false, fmt.Errorf("add peer %s: %w", uid, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add peer %s: %w", uid, err)
	}
	return n > 0, nil
}

// ── Membership ──────────────────────────────────────────────────────────────

// SetMembership records that a node was taken out of the mesh, or added back
// into it, and reports whether that changed anything here.
//
// This is the whole of removal. Dropping the rows would not be: the node would
// be learned back from the first neighbour that still knew it, or would
// register itself again with its next push, and there would be nothing left to
// tell the other nodes with. So the row stays as the record of the decision,
// carrying the time it was made, and every list of servers passes over it.
//
// An announcement about a node this one has never heard of writes the row all
// the same. A removal has to reach the nodes that were only ever going to meet
// the removed one later, or the mesh would readmit it through them.
//
// The decision is refused when the row already carries a later one, which is
// what makes the exchange convergent: the same announcements can arrive in any
// order, by any route, any number of times, and every node ends up holding the
// most recent decision about each of its neighbours. Self is never removable -
// a node that could be talked out of its own identity would lose its history
// and its place in every matrix on a single forged payload.
func (s *Store) SetMembership(uid string, removed bool, at model.Time) (bool, error) {
	// Removal also drops whatever made the node reachable or watched, so the
	// sync worker stops pushing to it and stops recording an outage for the
	// pushes that are no longer expected. Readmission deliberately restores
	// neither: the address comes back from the introduction, or from the peer
	// lists the neighbours answer this node's pushes with.
	res, err := s.db.Exec(`
		INSERT INTO "Servers" ("Uid","Name","Location","Url","IsSelf","Alerts","PushOnly","Removed","MemberAt","LastSeen")
		VALUES (?1,?1,'',NULL,0,0,0,?2,?3,?4)
		ON CONFLICT("Uid") DO UPDATE SET
			"Removed"=?2,
			"MemberAt"=?3,
			"Url"=CASE WHEN ?2 THEN NULL ELSE "Servers"."Url" END,
			"PushOnly"=CASE WHEN ?2 THEN 0 ELSE "Servers"."PushOnly" END
		WHERE "Servers"."IsSelf"=0 AND "Servers"."MemberAt"<?3`,
		uid, removed, at.DB(), model.Time{}.DB())
	if err != nil {
		return false, fmt.Errorf("set membership %s: %w", uid, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set membership %s: %w", uid, err)
	}
	return n > 0, nil
}

// Membership returns every decision this node holds, to be carried in its
// pushes. Not only the ones made on it: a removal has to cross the mesh whole,
// including to the nodes the removing one does not push to and the ones that
// were down when it happened, so each node repeats what it has been told.
// Repetition is free here, since applying a decision twice is applying it once.
//
// Rows with no decision recorded against them - every ordinary node, and the
// removals inherited from a database written before they were announced at all
// - are not published. An unstamped tombstone is one node's own business.
func (s *Store) Membership() ([]model.Removal, error) {
	rows, err := s.db.Query(`
		SELECT "Uid","MemberAt","Removed" FROM "Servers"
		WHERE "IsSelf"=0 AND "MemberAt">0 ORDER BY "MemberAt"`)
	if err != nil {
		return nil, fmt.Errorf("list membership: %w", err)
	}
	defer rows.Close()

	out := []model.Removal{}
	for rows.Next() {
		var (
			r  model.Removal
			at int64
		)
		if err := rows.Scan(&r.ServerID, &at, &r.Removed); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		r.Timestamp = model.FromDB(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// IsRemoved reports whether a node was taken out of this mesh, which is what
// the peer endpoints refuse a caller on.
func (s *Store) IsRemoved(uid string) (bool, error) {
	var removed bool
	err := s.db.QueryRow(`SELECT "Removed" FROM "Servers" WHERE "Uid"=?`, uid).Scan(&removed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check removal %s: %w", uid, err)
	}
	return removed, nil
}

// ForgetPeer stops this node syncing with a peer without recording a decision
// about it. It is what a node does with a peer that answered its push by saying
// this node is no longer in its mesh: the removal was somebody else's to make,
// so it is not re-announced from here - and it is left liftable, so that when
// the operator does add this node back, the first push from that peer is enough
// to make them neighbours again.
func (s *Store) ForgetPeer(id int64) error {
	if err := s.SetPeerURL(id, ""); err != nil {
		return err
	}
	return s.SetPushOnly(id, false)
}

// ── Servers, continued ──────────────────────────────────────────────────────

// TouchLastSeen records that a server was reachable at t.
func (s *Store) TouchLastSeen(id int64, t model.Time) error {
	_, err := s.db.Exec(`UPDATE "Servers" SET "LastSeen"=? WHERE "Id"=?`, t.DB(), id)
	if err != nil {
		return fmt.Errorf("touch last seen %d: %w", id, err)
	}
	return nil
}

const selectServer = `SELECT "Id","Uid","Name","Location",COALESCE("Url",''),"IsSelf","Alerts","PushOnly","Removed","LastSeen","Protocol" FROM "Servers"`

// present is what every listing adds to its own conditions: a removed node is
// not one of this mesh's servers any more, so it appears in no list, is probed
// by nothing, alerted on by nothing and drawn nowhere.
const present = ` WHERE "Removed"=0`

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

// ListServers returns every server still in the mesh, self included.
func (s *Store) ListServers() ([]model.Server, error) {
	return s.queryServers(selectServer + present + ` ORDER BY "IsSelf" DESC, "Name"`)
}

// ListPeers returns non-self servers that still have an address configured.
func (s *Store) ListPeers() ([]model.Server, error) {
	return s.queryServers(selectServer + present + ` AND "IsSelf"=0 AND "Url" IS NOT NULL AND "Url"<>'' ORDER BY "Name"`)
}

// ListPushOnly returns the peers that push to this node without being reachable
// from it, which are the ones it can only judge by what arrives.
func (s *Store) ListPushOnly() ([]model.Server, error) {
	return s.queryServers(selectServer + present + ` AND "IsSelf"=0 AND "PushOnly"=1 ORDER BY "Name"`)
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
		&srv.Alerts, &srv.PushOnly, &srv.Removed, &lastSeen, &srv.Protocol); err != nil {
		return model.Server{}, err
	}
	srv.LastSeen = model.FromDB(lastSeen)
	return srv, nil
}

// ── Metrics ─────────────────────────────────────────────────────────────────

// InsertMetrics appends samples for serverID and reports how many were
// written. A sample's volumes go in the same transaction, so a snapshot never
// exists without the disks it reported.
// The series store is written first, and the tables below it second. The
// order matters on failure: a series write is an upsert keyed by the reading's
// own identity, so repeating it changes nothing, while these tables accept the
// same sample twice. Writing the idempotent half first means a caller that
// retries after an error cannot end up with two copies of a snapshot.
func (s *Store) InsertMetrics(serverID int64, metrics []model.Metric) (int, error) {
	if len(metrics) == 0 {
		return 0, nil
	}

	if err := s.RecordSnapshots(serverID, metrics); err != nil {
		return 0, err
	}
	if !s.LegacyActive() {
		return len(metrics), nil
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

// InsertAvailability appends observations made by fromServerID. The series
// store is written first, for the reason given on InsertMetrics.
func (s *Store) InsertAvailability(fromServerID int64, records []Observation) error {
	if len(records) == 0 {
		return nil
	}

	if err := s.RecordProbes(fromServerID, records); err != nil {
		return err
	}
	if !s.LegacyActive() {
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

// Prune enforces retention on the two things that are not in the series store.
//
// The cutoffs differ because the tables do. The compatibility tables are
// written for a rollback that will not happen after the first day, so they are
// swept aggressively; what the mesh has already announced has to outlive the
// interval an alert repeats on, which an operator may set to days, so it is
// swept at whatever the configuration asks for.
func (s *Store) Prune(legacy, notices model.Time) error {
	if !s.LegacyActive() {
		return s.pruneNotices(notices)
	}
	cutoff := legacy

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
	return s.pruneNotices(notices)
}

// pruneNotices sweeps the one table that outlives the retirement of the other
// three: what the mesh has already announced is not a measurement, so it has
// no series to move into.
func (s *Store) pruneNotices(cutoff model.Time) error {
	if _, err := s.db.Exec(`DELETE FROM "AlertNotices" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune alert notices: %w", err)
	}
	return nil
}
