package store

import (
	"database/sql"
	"fmt"

	"github.com/m0n5ter/m0nit0r/internal/model"
)

// epochDays is the Julian day number of 1970-01-01, which is what turns
// SQLite's julianday() into a Unix count. The multiplication is done in
// SQLite's REAL, whose 53-bit mantissa leaves a Julian day accurate to well
// under a tenth of a millisecond, so the rounded result is exact.
const millisFromText = `CAST(ROUND((julianday(%s) - 2440587.5) * 86400000.0) AS INTEGER)`

// needsIntegerKeys reports whether the database still keys servers by the GUID
// they publish. "Uid" is the column that only exists once they are keyed by row
// id instead, so its absence from an existing table is the marker.
func needsIntegerKeys(db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM "sqlite_master" WHERE "type"='table' AND "name"='Servers'`).Scan(&n); err != nil {
		return false, fmt.Errorf("inspect schema: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	uid, err := hasColumn(db, "Servers", "Uid")
	if err != nil {
		return false, err
	}
	return !uid, nil
}

// toIntegerKeys rewrites a database that keys servers by GUID and stamps rows
// with ISO text into one that uses row ids and Unix milliseconds.
//
// Every table is rebuilt beside the original and swapped in, rather than
// altered in place, because the columns change type and the keys change value.
// MetricDisks is the exception: it is keyed by the snapshot's row id, the
// copies keep those ids, so the volumes need no rewriting at all.
//
// The whole thing runs in one transaction. SQLite makes DDL transactional too,
// so a failure at any point leaves the file exactly as it was found and Open
// reports it rather than half-migrating a node's history.
func toIntegerKeys(db *sql.DB) error {
	// Renaming a table normally makes SQLite reparse the whole schema and
	// rewrite every reference to the old name. Here that is neither wanted nor
	// possible: the new tables are written referencing the names they are about
	// to be given, and MetricDisks points at a "MetricSnapshots" that does not
	// exist for the moment between the drop and the rename.
	if _, err := db.Exec(`PRAGMA legacy_alter_table=ON`); err != nil {
		return fmt.Errorf("legacy_alter_table: %w", err)
	}
	defer db.Exec(`PRAGMA legacy_alter_table=OFF`)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		`CREATE TABLE "Servers_new" (
			"Id"       INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
			"Uid"      TEXT NOT NULL UNIQUE,
			"Name"     TEXT NOT NULL,
			"Location" TEXT NOT NULL,
			"Url"      TEXT NULL,
			"IsSelf"   INTEGER NOT NULL,
			"LastSeen" INTEGER NOT NULL
		)`,
		`INSERT INTO "Servers_new" ("Uid","Name","Location","Url","IsSelf","LastSeen")
		 SELECT "Id","Name","Location","Url","IsSelf",` + fmt.Sprintf(millisFromText, `"LastSeen"`) + `
		 FROM "Servers"`,

		`CREATE TABLE "MetricSnapshots_new" (
			"Id"            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
			"ServerId"      INTEGER NOT NULL REFERENCES "Servers"("Id"),
			"Timestamp"     INTEGER NOT NULL,
			"CpuPercent"    REAL NOT NULL,
			"CpuTempC"      REAL NULL,
			"MemoryPercent" REAL NOT NULL,
			"MemoryTotalMb" REAL NOT NULL,
			"MemoryUsedMb"  REAL NOT NULL,
			"UptimeSeconds" REAL NOT NULL
		)`,
		// The row ids are carried over unchanged, which is what lets MetricDisks
		// stay where it is. A snapshot whose server is not in the register has
		// nothing to point at and is dropped; the orphaned volumes go below.
		`INSERT INTO "MetricSnapshots_new"
			("Id","ServerId","Timestamp","CpuPercent","CpuTempC","MemoryPercent","MemoryTotalMb","MemoryUsedMb","UptimeSeconds")
		 SELECT m."Id", s."Id",` + fmt.Sprintf(millisFromText, `m."Timestamp"`) + `,
			m."CpuPercent", m."CpuTempC", m."MemoryPercent", m."MemoryTotalMb", m."MemoryUsedMb", m."UptimeSeconds"
		 FROM "MetricSnapshots" m
		 JOIN "Servers_new" s ON s."Uid"=m."ServerId"`,

		`CREATE TABLE "AvailabilityRecords_new" (
			"Id"           INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
			"FromServerId" INTEGER NOT NULL REFERENCES "Servers"("Id"),
			"ToServerId"   INTEGER NOT NULL REFERENCES "Servers"("Id"),
			"Timestamp"    INTEGER NOT NULL,
			"IsAvailable"  INTEGER NOT NULL,
			"LatencyMs"    REAL NULL,
			"HttpStatus"   INTEGER NULL
		)`,
		`INSERT INTO "AvailabilityRecords_new"
			("Id","FromServerId","ToServerId","Timestamp","IsAvailable","LatencyMs","HttpStatus")
		 SELECT a."Id", f."Id", t."Id",` + fmt.Sprintf(millisFromText, `a."Timestamp"`) + `,
			a."IsAvailable", a."LatencyMs", a."HttpStatus"
		 FROM "AvailabilityRecords" a
		 JOIN "Servers_new" f ON f."Uid"=a."FromServerId"
		 JOIN "Servers_new" t ON t."Uid"=a."ToServerId"`,

		`DROP TABLE "Servers"`,
		`DROP TABLE "MetricSnapshots"`,
		`DROP TABLE "AvailabilityRecords"`,

		`ALTER TABLE "Servers_new" RENAME TO "Servers"`,
		`ALTER TABLE "MetricSnapshots_new" RENAME TO "MetricSnapshots"`,
		`ALTER TABLE "AvailabilityRecords_new" RENAME TO "AvailabilityRecords"`,

		`DELETE FROM "MetricDisks" WHERE "MetricId" NOT IN (SELECT "Id" FROM "MetricSnapshots")`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%w, running: %s", err, stmt)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// The rebuilt tables are around forty per cent smaller, but the pages the
	// originals occupied are only marked free, so the file itself does not
	// shrink until it is rewritten. This is the one moment where most of it is
	// free space rather than history a few days from being pruned, and
	// retention caps how much there is to rewrite, so it is worth the seconds.
	//
	// Whether it succeeds is not worth failing over: the migration is committed
	// either way, and a node that will not start because it could not compact a
	// file it can otherwise read would be the worse outcome.
	db.Exec(`VACUUM`)
	return nil
}

// addColumns brings a table created by an earlier build up to the current
// shape. CREATE TABLE IF NOT EXISTS leaves an existing table exactly as it
// found it, so a column added to the schema after a node was deployed has to be
// applied separately.
//
// Unlike toIntegerKeys this needs no data rewritten: every column here carries
// a DEFAULT, which is what SQLite fills the existing rows with. A node whose
// register predates the alerting flag reads back as one where nobody alerts,
// which is exactly what was true of it.
func addColumns(db *sql.DB) error {
	for _, col := range []struct{ table, name, decl string }{
		{"Servers", "Alerts", `"Alerts" INTEGER NOT NULL DEFAULT 0`},
		{"Servers", "PushOnly", `"PushOnly" INTEGER NOT NULL DEFAULT 0`},
		{"Servers", "Removed", `"Removed" INTEGER NOT NULL DEFAULT 0`},
		{"Servers", "MemberAt", `"MemberAt" INTEGER NOT NULL DEFAULT 0`},
		// Which revision of the peer protocol a node last spoke here. Zero is
		// a node that has never pushed, or one old enough to send no version
		// at all - which are the same thing for the only decision this drives:
		// whether every peer is new enough that the tables the first build
		// stored its readings in have stopped being anybody's history.
		{"Servers", "Protocol", `"Protocol" INTEGER NOT NULL DEFAULT 0`},
	} {
		present, err := hasColumn(db, col.table, col.name)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE "` + col.table + `" ADD COLUMN ` + col.decl); err != nil {
			return fmt.Errorf("add %s.%s: %w", col.table, col.name, err)
		}
		if col.name == "Removed" {
			if err := markLegacyRemovals(db); err != nil {
				return err
			}
		}
	}
	return nil
}

// markLegacyRemovals turns the peers an earlier build had dropped into the
// tombstones this one keeps them as, and is run once, on the database where the
// column was just added.
//
// That build recorded a removal by clearing the peer's address and nothing
// else, and told a neighbour's introduction from a removed peer apart by how
// the row looked: a node nobody had been introduced to yet stood in for its own
// name and had never been reached, so a row that had a name of its own and a
// time it was last reached, and no address, was one somebody had taken out.
// Reading that off the shape of a row is exactly what the tombstone replaces,
// so the inference is made one final time here and written down.
//
// The decisions are left unstamped, which keeps them off the wire. They were
// never announced when they were made, the nodes they concern may well have
// been legitimately re-added elsewhere in the months since, and a build being
// upgraded is no occasion to broadcast a removal nobody has just asked for.
func markLegacyRemovals(db *sql.DB) error {
	// The stamp an unmet node is registered with is the zero time, which is a
	// long way from zero milliseconds - so it is compared against by value
	// rather than assumed to be falsy.
	_, err := db.Exec(`
		UPDATE "Servers" SET "Removed"=1
		WHERE "IsSelf"=0 AND "PushOnly"=0 AND "Url" IS NULL
			AND "LastSeen"<>? AND "Name"<>"Uid"`, model.Time{}.DB())
	if err != nil {
		return fmt.Errorf("mark legacy removals: %w", err)
	}
	return nil
}
