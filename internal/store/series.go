package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/series"
)

// Sample is one reading about one series, as this database holds it: both
// dimensions resolved to local row ids, and the value already scaled to the
// integer the column stores.
//
// Value is nil when the reading was attempted and produced no number. That is
// not the same as the row being missing, and the difference is the whole of
// what the counters in a bucket are built from: a probe that timed out has no
// latency to average, a push-only peer that answered was never timed at all,
// and neither is an absence of a check.
type Sample struct {
	Param     series.Param
	Server1   int64
	Server2   int64
	Volume    int64
	Timestamp model.Time
	Value     *int64
	Ok        bool
}

// Bucket is one aggregated interval of one series. See the Rollups table for
// what the three counters are each for.
type Bucket struct {
	Param    series.Param
	Server1  int64
	Server2  int64
	Volume   int64
	Level    series.Level
	Bucket   int64
	Mn       *int64
	Mx       *int64
	Sum      int64
	Count    int64
	CountOk  int64
	CountVal int64
}

// Mean is the bucket's average reading, decoded. The bool is false when
// nothing in the bucket produced a number, which a chart draws as a gap rather
// than as a zero.
func (b Bucket) Mean() (float64, bool) {
	if b.CountVal <= 0 {
		return 0, false
	}
	return series.DecodeSum(b.Param, b.Sum, b.CountVal), true
}

// Value is what the bucket reports as its reading, which for a counter is the
// last one in it rather than the average of the run. See series.Rule.
func (b Bucket) Value() (float64, bool) {
	spec, known := series.Of(b.Param)
	if known && spec.Rule == series.Last {
		if b.Mx == nil {
			return 0, false
		}
		return series.Decode(b.Param, *b.Mx), true
	}
	return b.Mean()
}

// ── Volumes ─────────────────────────────────────────────────────────────────

// VolumeID maps a drive's name on a server to the small integer the readings
// about it are keyed by, registering it the first time it is seen.
//
// Cached in memory because it is asked on every sample of every disk of every
// node, and the answer never changes: ids are handed out and never reused, and
// a drive that disappears simply stops being asked about.
func (s *Store) VolumeID(serverID int64, name string) (int64, error) {
	key := volumeKey{serverID, name}

	s.volumeMu.RLock()
	id, ok := s.volumeIDs[key]
	s.volumeMu.RUnlock()
	if ok {
		return id, nil
	}

	// The conflict clause updates the name to the value it already holds: a
	// no-op whose only purpose is to make RETURNING yield the existing row.
	err := s.db.QueryRow(`
		INSERT INTO "Volumes" ("ServerId","Name") VALUES (?,?)
		ON CONFLICT("ServerId","Name") DO UPDATE SET "Name"=excluded."Name"
		RETURNING "Id"`, serverID, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("resolve volume %q on %d: %w", name, serverID, err)
	}

	s.volumeMu.Lock()
	if s.volumeIDs == nil {
		s.volumeIDs = map[volumeKey]int64{}
	}
	s.volumeIDs[key] = id
	s.volumeMu.Unlock()
	return id, nil
}

type volumeKey struct {
	serverID int64
	name     string
}

// volumeCache is embedded in Store. It is a separate type only so that Store's
// own declaration stays about the database handle.
type volumeCache struct {
	volumeMu  sync.RWMutex
	volumeIDs map[volumeKey]int64
}

// ── Writing ─────────────────────────────────────────────────────────────────

// InsertSamples writes readings, replacing any already held for the same
// series and instant.
//
// Replacing rather than ignoring is what makes a resend harmless: peers offer
// an overlapping window every round, and a reading that arrives twice is the
// same reading. It is also why no high-water mark is needed on this path - the
// second copy costs a page write and changes nothing.
func (s *Store) InsertSamples(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin samples tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO "Samples" ("Param","Server1","Server2","Volume","Timestamp","Value","IsOk")
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT("Server1","Param","Server2","Volume","Timestamp") DO UPDATE SET
			"Value"=excluded."Value", "IsOk"=excluded."IsOk"`)
	if err != nil {
		return fmt.Errorf("prepare sample insert: %w", err)
	}
	defer stmt.Close()

	for _, smp := range samples {
		if _, err := stmt.Exec(int64(smp.Param), smp.Server1, smp.Server2, smp.Volume,
			smp.Timestamp.DB(), smp.Value, smp.Ok); err != nil {
			return fmt.Errorf("insert sample: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit samples: %w", err)
	}
	return nil
}

// UpsertRollups writes aggregates computed elsewhere - which means: by the
// node that owns the series, and carried here in its pushes.
//
// The incoming bucket replaces whatever is held. A node only ever publishes a
// bucket once it can no longer change, so two copies of one are two copies of
// the same arithmetic, and taking the newer one is both correct and the only
// rule that needs no agreement about who computed it first.
func (s *Store) UpsertRollups(buckets []Bucket) error {
	if len(buckets) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin rollups tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO "Rollups"
			("Param","Server1","Server2","Volume","Level","Bucket","Mn","Mx","Sum","Count","CountOk","CountVal")
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT("Server1","Param","Server2","Volume","Level","Bucket") DO UPDATE SET
			"Mn"=excluded."Mn", "Mx"=excluded."Mx", "Sum"=excluded."Sum",
			"Count"=excluded."Count", "CountOk"=excluded."CountOk", "CountVal"=excluded."CountVal"`)
	if err != nil {
		return fmt.Errorf("prepare rollup insert: %w", err)
	}
	defer stmt.Close()

	for _, b := range buckets {
		if _, err := stmt.Exec(int64(b.Param), b.Server1, b.Server2, b.Volume, int64(b.Level),
			b.Bucket, b.Mn, b.Mx, b.Sum, b.Count, b.CountOk, b.CountVal); err != nil {
			return fmt.Errorf("insert rollup: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollups: %w", err)
	}
	return nil
}

// ── Rolling up ──────────────────────────────────────────────────────────────

// ownSeries restricts aggregation to the series this node is entitled to
// compute: its own, and those of peers too old to compute their own.
//
// A peer new enough to send aggregates has already sealed them, and
// recomputing them here from the handful of samples that happened to arrive
// would replace a whole bucket with the fraction of it this node witnessed.
// A peer too old to send them offers nothing but samples, so for those the
// only aggregate that will ever exist is the one made here.
const ownSeries = ` AND "Server1" NOT IN (
	SELECT "Id" FROM "Servers" WHERE "IsSelf"=0 AND "Protocol">=2)`

// RollSamples aggregates raw samples into the finest level, for whole buckets
// in [from, to). It reports how many rows it wrote.
//
// Running it twice over the same range is running it once: every bucket is
// recomputed from the samples in full and written over whatever was there.
// That is what lets the caller keep a single watermark and be careless with it
// - falling behind costs a rescan, never a wrong number.
func (s *Store) RollSamples(from, to int64) (int64, error) {
	level := series.Ladder[0].Level
	width := series.Width(level).Milliseconds()

	res, err := s.db.Exec(`
		INSERT INTO "Rollups"
			("Param","Server1","Server2","Volume","Level","Bucket","Mn","Mx","Sum","Count","CountOk","CountVal")
		SELECT "Param","Server1","Server2","Volume",?1,"Timestamp"/?2,
			MIN("Value"), MAX("Value"), COALESCE(SUM("Value"),0),
			COUNT(*), COALESCE(SUM("IsOk"),0), COUNT("Value")
		FROM "Samples"
		WHERE "Timestamp">=?3 AND "Timestamp"<?4`+ownSeries+`
		GROUP BY "Server1","Param","Server2","Volume","Timestamp"/?2
		ON CONFLICT("Server1","Param","Server2","Volume","Level","Bucket") DO UPDATE SET
			"Mn"=excluded."Mn", "Mx"=excluded."Mx", "Sum"=excluded."Sum",
			"Count"=excluded."Count", "CountOk"=excluded."CountOk", "CountVal"=excluded."CountVal"`,
		int64(level), width, from*width, to*width)
	if err != nil {
		return 0, fmt.Errorf("roll samples [%d,%d): %w", from, to, err)
	}
	return res.RowsAffected()
}

// RollLevel builds one rung of the ladder from the one below it, for whole
// buckets of the coarser level in [from, to).
//
// The widths nest exactly - thirty seconds ten times into five minutes, five
// minutes twelve times into an hour, an hour twenty-four times into a day - so
// dividing the finer bucket number by that ratio names the coarser bucket it
// belongs to, and no reading is ever split across two.
//
// Sums add, counts add, the extremes are the extremes of the extremes. This is
// the whole reason the table stores a sum rather than an average: none of it
// needs to know how full any of the finer buckets was.
func (s *Store) RollLevel(fine, coarse series.Level, from, to int64) (int64, error) {
	fineWidth := series.Width(fine).Milliseconds()
	coarseWidth := series.Width(coarse).Milliseconds()
	if fineWidth <= 0 || coarseWidth <= 0 || coarseWidth%fineWidth != 0 {
		return 0, fmt.Errorf("level %d does not divide level %d", fine, coarse)
	}
	ratio := coarseWidth / fineWidth

	res, err := s.db.Exec(`
		INSERT INTO "Rollups"
			("Param","Server1","Server2","Volume","Level","Bucket","Mn","Mx","Sum","Count","CountOk","CountVal")
		SELECT "Param","Server1","Server2","Volume",?1,"Bucket"/?2,
			MIN("Mn"), MAX("Mx"), COALESCE(SUM("Sum"),0),
			COALESCE(SUM("Count"),0), COALESCE(SUM("CountOk"),0), COALESCE(SUM("CountVal"),0)
		FROM "Rollups"
		WHERE "Level"=?3 AND "Bucket">=?4 AND "Bucket"<?5`+ownSeries+`
		GROUP BY "Server1","Param","Server2","Volume","Bucket"/?2
		ON CONFLICT("Server1","Param","Server2","Volume","Level","Bucket") DO UPDATE SET
			"Mn"=excluded."Mn", "Mx"=excluded."Mx", "Sum"=excluded."Sum",
			"Count"=excluded."Count", "CountOk"=excluded."CountOk", "CountVal"=excluded."CountVal"`,
		int64(coarse), ratio, int64(fine), from*ratio, to*ratio)
	if err != nil {
		return 0, fmt.Errorf("roll level %d into %d: %w", fine, coarse, err)
	}
	return res.RowsAffected()
}

// OldestSampleBucket names the earliest level-zero bucket any surviving sample
// falls in. The bool is false when there are no samples at all, which is a
// node that has just started.
func (s *Store) OldestSampleBucket() (int64, bool, error) {
	width := series.Width(series.Ladder[0].Level).Milliseconds()
	return s.scalar(`SELECT MIN("Timestamp")/? FROM "Samples"`, "oldest sample", width)
}

// OldestRollupBucket names the earliest bucket held at a level.
func (s *Store) OldestRollupBucket(level series.Level) (int64, bool, error) {
	return s.scalar(`SELECT MIN("Bucket") FROM "Rollups" WHERE "Level"=?`, "oldest rollup", int64(level))
}

// scalar runs a query yielding one nullable integer, reporting through the
// bool whether there was a row to take it from.
func (s *Store) scalar(query, what string, args ...any) (int64, bool, error) {
	var raw sql.NullInt64
	if err := s.db.QueryRow(query, args...).Scan(&raw); err != nil {
		return 0, false, fmt.Errorf("%s: %w", what, err)
	}
	return raw.Int64, raw.Valid, nil
}

// ── Watermarks ──────────────────────────────────────────────────────────────

// MetaGet reads a single stored fact. The bool is false when it was never
// written, which is what a fresh database looks like.
func (s *Store) MetaGet(key string) (int64, bool, error) {
	var v int64
	err := s.db.QueryRow(`SELECT "Value" FROM "Meta" WHERE "Key"=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read meta %q: %w", key, err)
	}
	return v, true, nil
}

// MetaSet writes one.
func (s *Store) MetaSet(key string, value int64) error {
	_, err := s.db.Exec(`
		INSERT INTO "Meta" ("Key","Value") VALUES (?,?)
		ON CONFLICT("Key") DO UPDATE SET "Value"=excluded."Value"`, key, value)
	if err != nil {
		return fmt.Errorf("write meta %q: %w", key, err)
	}
	return nil
}

// ── Retention ───────────────────────────────────────────────────────────────

// PruneSamples drops raw readings older than cutoff.
//
// The table holds twenty minutes, so this is a scan of something small and
// needs no index of its own - which is also why the samples are not in the
// same table as the aggregates, where a scan would mean walking a year.
func (s *Store) PruneSamples(cutoff model.Time) error {
	if _, err := s.db.Exec(`DELETE FROM "Samples" WHERE "Timestamp"<?`, cutoff.DB()); err != nil {
		return fmt.Errorf("prune samples: %w", err)
	}
	return nil
}

// PruneRollups drops aggregates at one level older than a bucket.
func (s *Store) PruneRollups(level series.Level, before int64) error {
	_, err := s.db.Exec(`DELETE FROM "Rollups" WHERE "Level"=? AND "Bucket"<?`, int64(level), before)
	if err != nil {
		return fmt.Errorf("prune rollups at level %d: %w", level, err)
	}
	return nil
}

// DropVolumes forgets drives nothing holds a reading about any more, so that a
// USB disk plugged in once does not stay in the dimension table for ever.
func (s *Store) DropVolumes() error {
	res, err := s.db.Exec(`
		DELETE FROM "Volumes" WHERE "Id" NOT IN (SELECT DISTINCT "Volume" FROM "Rollups")
			AND "Id" NOT IN (SELECT DISTINCT "Volume" FROM "Samples")`)
	if err != nil {
		return fmt.Errorf("prune volumes: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// The cache maps names to ids that no longer exist. Dropped whole
		// rather than picked over: it refills from one query per live drive.
		s.volumeMu.Lock()
		s.volumeIDs = nil
		s.volumeMu.Unlock()
	}
	return nil
}

// ── Recording ───────────────────────────────────────────────────────────────

// RecordSnapshots turns whole-machine samples into the rows the series store
// holds, resolving each drive's name to its id.
//
// A reading the host could not produce - a CPU with no readable sensor, a
// drive that reports no temperature - is left out rather than written as a row
// with no value. Every five seconds, on every node, an absent sensor would
// otherwise cost a row per sample to say nothing; the parameter simply has no
// series on that machine, and a gap in a chart is what its absence looks like
// either way. A check that was made and failed is a different matter, and that
// one is written - see RecordProbe.
func (s *Store) RecordSnapshots(serverID int64, metrics []model.Metric) error {
	samples := make([]Sample, 0, len(metrics)*8)

	put := func(at model.Time, p series.Param, volume int64, v float64) {
		scaled := series.Encode(p, v)
		samples = append(samples, Sample{
			Param: p, Server1: serverID, Volume: volume,
			Timestamp: at, Value: &scaled, Ok: true,
		})
	}

	for _, m := range metrics {
		put(m.Timestamp, series.CPUPercent, 0, m.CpuPercent)
		put(m.Timestamp, series.MemoryPercent, 0, m.MemoryPercent)
		put(m.Timestamp, series.MemoryTotalMb, 0, m.MemoryTotalMb)
		put(m.Timestamp, series.MemoryUsedMb, 0, m.MemoryUsedMb)
		put(m.Timestamp, series.UptimeSeconds, 0, m.UptimeSeconds)
		if m.CpuTempC != nil {
			put(m.Timestamp, series.CPUTempC, 0, *m.CpuTempC)
		}

		for _, d := range m.Disks {
			if d.Name == "" {
				continue
			}
			volume, err := s.VolumeID(serverID, d.Name)
			if err != nil {
				return err
			}
			put(m.Timestamp, series.DiskTotalGb, volume, d.TotalGb)
			put(m.Timestamp, series.DiskUsedGb, volume, d.UsedGb)
			if d.TempC != nil {
				put(m.Timestamp, series.DiskTempC, volume, *d.TempC)
			}
		}
	}

	return s.InsertSamples(samples)
}

// RecordProbes turns reachability observations into samples on the edges they
// were made along.
//
// A failed check carries no latency, whatever was measured. What a timed-out
// probe records is how long this node was willing to wait, which is a property
// of the timeout and not of the route, and letting it into the average would
// put a spike in every latency chart at exactly the moment the chart has
// nothing to say. The check itself is still written, with no value: that is
// what the availability counter is made of.
func (s *Store) RecordProbes(fromServerID int64, records []Observation) error {
	samples := make([]Sample, 0, len(records))
	for _, r := range records {
		smp := Sample{
			Param:     series.LatencyMs,
			Server1:   fromServerID,
			Server2:   r.ToServerID,
			Timestamp: r.Timestamp,
			Ok:        r.IsAvailable,
		}
		if r.IsAvailable && r.LatencyMs != nil {
			scaled := series.Encode(series.LatencyMs, *r.LatencyMs)
			smp.Value = &scaled
		}
		samples = append(samples, smp)
	}
	return s.InsertSamples(samples)
}

// ── Protocol ────────────────────────────────────────────────────────────────

// SetProtocol records which revision of the peer protocol a node last spoke.
// Only ever raised within a run, so that one malformed or downgraded push
// cannot make a mesh look older than it is.
func (s *Store) SetProtocol(id int64, version int) error {
	_, err := s.db.Exec(`UPDATE "Servers" SET "Protocol"=?1 WHERE "Id"=?2 AND "Protocol"<?1`, version, id)
	if err != nil {
		return fmt.Errorf("set protocol %d: %w", id, err)
	}
	return nil
}

// MeshSpeaks reports whether every peer this node exchanges data with speaks at
// least the given revision, and how many do not.
//
// The question it answers is whether the tables the first build stored
// readings in are still anybody's history. Only the nodes that actually
// exchange data count: a row standing in for an id somebody mentioned once has
// never pushed here and never will, and waiting on it would mean waiting for
// ever.
func (s *Store) MeshSpeaks(version int) (bool, int, error) {
	var behind int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM "Servers"
		WHERE "IsSelf"=0 AND "Removed"=0 AND "Protocol"<?
			AND (("Url" IS NOT NULL AND "Url"<>'') OR "PushOnly"=1)`, version).Scan(&behind)
	if err != nil {
		return false, 0, fmt.Errorf("check mesh protocol: %w", err)
	}
	return behind == 0, behind, nil
}

// ── Retiring the original tables ────────────────────────────────────────────

// seriesSince names when this database first had a series store, which is what
// the grace before the original tables are dropped is measured from.
const seriesSince = "series.since"

// LegacyActive reports whether the tables the first build stored readings in
// are still here and still being written.
func (s *Store) LegacyActive() bool {
	s.legacyMu.RLock()
	defer s.legacyMu.RUnlock()
	return s.legacy
}

// LegacyAge is how long this database has had a series store. The bool is
// false before that was ever recorded, which cannot happen after an Open but
// is not worth being confident about.
func (s *Store) LegacyAge(now model.Time) (time.Duration, bool, error) {
	since, ok, err := s.MetaGet(seriesSince)
	if err != nil || !ok {
		return 0, false, err
	}
	return now.Sub(model.FromDB(since).Time), true, nil
}

// RetireLegacy drops the tables the first build stored readings in.
//
// Nothing reads them any more: every chart, the matrix, the cards and the
// alert thresholds are answered from the series store, and the peer protocol
// fills the original form of a payload from it too. What they are kept for
// after that is one thing only - a build rolled back inside the grace window
// finds its history where it left it. Past that window they are several
// hundred megabytes of a question nobody asks.
//
// A node that is rolled back afterwards creates them again, empty, and runs.
// It loses the history, which is the price of the window having passed.
func (s *Store) RetireLegacy() error {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS "MetricDisks"`,
		`DROP TABLE IF EXISTS "MetricSnapshots"`,
		`DROP TABLE IF EXISTS "AvailabilityRecords"`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("retire legacy tables: %w, running: %s", err, stmt)
		}
	}

	s.legacyMu.Lock()
	s.legacy = false
	s.legacyMu.Unlock()

	// The pages those tables held are free now but the file is not smaller,
	// and this is the one moment where most of it is free space rather than
	// history a day from being pruned. Whether it succeeds is not worth
	// failing over: the tables are gone either way, and a node that would not
	// run because it could not compact a file it can otherwise read would be
	// the worse outcome.
	s.db.Exec(`VACUUM`)
	return nil
}

// legacyState is embedded in Store, beside the volume cache.
type legacyState struct {
	legacyMu sync.RWMutex
	legacy   bool
}
