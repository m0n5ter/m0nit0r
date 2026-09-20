package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/series"
)

// This file is the reading half of the series store. Two rules run through all
// of it.
//
// The first is which table answers. Aggregates lag: a bucket is not written
// until it can no longer change, which is minutes after it ends, so anything
// that has to be current - the matrix, a node's card, the alert thresholds,
// the live chart - reads samples, and only the ranges wider than the samples
// are kept read aggregates. The second is that a missing row and a row with no
// value are different answers, and both are different from zero: a host with
// no temperature sensor, a probe that timed out, and a drive at freezing point
// have to stay distinguishable all the way to the chart.

// ── One node's readings ─────────────────────────────────────────────────────

// MetricsRaw rebuilds a server's samples, newest last, at the cadence they
// were taken. This is the live view, where the five-second spacing is the
// point.
func (s *Store) MetricsRaw(serverID int64, since model.Time) ([]model.Metric, error) {
	rows, err := s.db.Query(`
		SELECT s."Timestamp", s."Param", COALESCE(v."Name",''), s."Value"
		FROM "Samples" s LEFT JOIN "Volumes" v ON v."Id"=s."Volume"
		WHERE s."Server1"=? AND s."Server2"=0 AND s."Timestamp">=?`, serverID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("raw metrics %d: %w", serverID, err)
	}
	defer rows.Close()

	a := newAssembler()
	for rows.Next() {
		var (
			at     int64
			param  int64
			volume string
			value  sql.NullInt64
		)
		if err := rows.Scan(&at, &param, &volume, &value); err != nil {
			return nil, fmt.Errorf("scan raw metric: %w", err)
		}
		if !value.Valid {
			continue
		}
		a.put(at, series.Param(param), volume, series.Decode(series.Param(param), value.Int64))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return a.metrics(func(at int64) model.Time { return model.FromDB(at) }), nil
}

// MetricsAtLevel rebuilds a server's history from the aggregates at one rung
// of the ladder, over whole buckets in [from, to].
//
// Each bucket becomes one sample stamped at its end, which is the instant the
// reading is true as of. Stamping it at the start would draw every chart
// shifted a bucket into the past.
func (s *Store) MetricsAtLevel(serverID int64, level series.Level, from, to int64) ([]model.Metric, error) {
	rows, err := s.db.Query(`
		SELECT r."Bucket", r."Param", COALESCE(v."Name",''), r."Mx", r."Sum", r."CountVal"
		FROM "Rollups" r LEFT JOIN "Volumes" v ON v."Id"=r."Volume"
		WHERE r."Server1"=? AND r."Server2"=0 AND r."Level"=? AND r."Bucket">=? AND r."Bucket"<=?`,
		serverID, int64(level), from, to)
	if err != nil {
		return nil, fmt.Errorf("level metrics %d: %w", serverID, err)
	}
	defer rows.Close()

	a := newAssembler()
	for rows.Next() {
		var (
			bucket   int64
			param    int64
			volume   string
			mx       sql.NullInt64
			sum      int64
			countVal int64
		)
		if err := rows.Scan(&bucket, &param, &volume, &mx, &sum, &countVal); err != nil {
			return nil, fmt.Errorf("scan level metric: %w", err)
		}
		b := Bucket{Param: series.Param(param), Sum: sum, CountVal: countVal}
		if mx.Valid {
			b.Mx = &mx.Int64
		}
		v, ok := b.Value()
		if !ok {
			continue
		}
		a.put(bucket, b.Param, volume, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The bucket is reported at the moment it closes, less the millisecond
	// that already belongs to the next one.
	return a.metrics(func(bucket int64) model.Time {
		return model.FromDB(series.BucketEnd(bucket, level) - 1)
	}), nil
}

// LatestMetricSeries returns a server's newest reading of each parameter,
// assembled into the one sample the dashboard's card shows.
//
// A bare column beside MAX() takes its value from the row that produced the
// maximum, which is what makes this one pass rather than a query per
// parameter. Only one aggregate may appear for that to be defined, so the
// timestamp is the maximum and everything else is bare.
func (s *Store) LatestMetricSeries(serverID int64) (*model.Metric, error) {
	rows, err := s.db.Query(`
		SELECT MAX(s."Timestamp"), s."Param", COALESCE(v."Name",''), s."Value"
		FROM "Samples" s LEFT JOIN "Volumes" v ON v."Id"=s."Volume"
		WHERE s."Server1"=? AND s."Server2"=0
		GROUP BY s."Param", s."Volume"`, serverID)
	if err != nil {
		return nil, fmt.Errorf("latest series %d: %w", serverID, err)
	}
	defer rows.Close()

	a := newAssembler()
	var newest int64
	found := false
	for rows.Next() {
		var (
			at     int64
			param  int64
			volume string
			value  sql.NullInt64
		)
		if err := rows.Scan(&at, &param, &volume, &value); err != nil {
			return nil, fmt.Errorf("scan latest series: %w", err)
		}
		if !value.Valid {
			continue
		}
		found = true
		if at > newest {
			newest = at
		}
		// Collapsed onto one instant: these are the newest readings of
		// different parameters, taken seconds apart at most, and the card
		// shows them as one state of the machine.
		a.put(0, series.Param(param), volume, series.Decode(series.Param(param), value.Int64))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	out := a.metrics(func(int64) model.Time { return model.FromDB(newest) })
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// ── One edge's reachability ─────────────────────────────────────────────────

// EdgePoint is one interval of one route, as a chart draws it: how much of it
// was reachable, and what it cost when it was.
//
// LatencyMs is nil when nothing in the interval was timed - every check failed,
// or the peer is push-only and is watched rather than probed. Availability is
// the fraction of checks that succeeded, which for a raw sample is one or zero.
type EdgePoint struct {
	Timestamp    model.Time
	Availability float64
	IsAvailable  bool
	LatencyMs    *float64
	MaxLatencyMs *float64
}

// EdgeRaw returns one route's checks from since onwards, one point per check.
func (s *Store) EdgeRaw(fromServerID, toServerID int64, since model.Time) ([]EdgePoint, error) {
	rows, err := s.db.Query(`
		SELECT "Timestamp","Value","IsOk" FROM "Samples"
		WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0 AND "Timestamp">=?
		ORDER BY "Timestamp"`,
		fromServerID, int64(series.LatencyMs), toServerID, since.DB())
	if err != nil {
		return nil, fmt.Errorf("raw edge %d->%d: %w", fromServerID, toServerID, err)
	}
	defer rows.Close()

	out := []EdgePoint{}
	for rows.Next() {
		var (
			at    int64
			value sql.NullInt64
			ok    bool
		)
		if err := rows.Scan(&at, &value, &ok); err != nil {
			return nil, fmt.Errorf("scan raw edge: %w", err)
		}
		p := EdgePoint{Timestamp: model.FromDB(at), IsAvailable: ok}
		if ok {
			p.Availability = 1
		}
		if value.Valid {
			ms := series.Decode(series.LatencyMs, value.Int64)
			p.LatencyMs, p.MaxLatencyMs = &ms, &ms
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EdgeAtLevel returns one route's history from the aggregates, one point per
// bucket over [from, to].
//
// A bucket counts as available when any check in it succeeded. The alternative
// - available only when every check did - would paint a whole day red for one
// lost probe, and the fraction is carried alongside for whoever wants to know
// how much of the interval it really was.
func (s *Store) EdgeAtLevel(fromServerID, toServerID int64, level series.Level, from, to int64) ([]EdgePoint, error) {
	rows, err := s.db.Query(`
		SELECT "Bucket","Mx","Sum","Count","CountOk","CountVal" FROM "Rollups"
		WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0
			AND "Level"=? AND "Bucket">=? AND "Bucket"<=?
		ORDER BY "Bucket"`,
		fromServerID, int64(series.LatencyMs), toServerID, int64(level), from, to)
	if err != nil {
		return nil, fmt.Errorf("level edge %d->%d: %w", fromServerID, toServerID, err)
	}
	defer rows.Close()

	out := []EdgePoint{}
	for rows.Next() {
		var (
			bucket                    int64
			mx                        sql.NullInt64
			sum, count, okCount, vals int64
		)
		if err := rows.Scan(&bucket, &mx, &sum, &count, &okCount, &vals); err != nil {
			return nil, fmt.Errorf("scan level edge: %w", err)
		}
		p := EdgePoint{
			Timestamp:   model.FromDB(series.BucketEnd(bucket, level) - 1),
			IsAvailable: okCount > 0,
		}
		if count > 0 {
			p.Availability = float64(okCount) / float64(count)
		}
		if vals > 0 {
			mean := series.DecodeSum(series.LatencyMs, sum, vals)
			p.LatencyMs = &mean
		}
		if mx.Valid {
			peak := series.Decode(series.LatencyMs, mx.Int64)
			p.MaxLatencyMs = &peak
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── The matrix ──────────────────────────────────────────────────────────────

// EdgeState is the current standing of one route, as the matrix draws a cell.
type EdgeState struct {
	FromUID      string
	ToUID        string
	LastCheck    model.Time
	Checks       int
	Successes    int
	LastLatency  *float64
	LastWasUp    bool
	LastAttempt  model.Time
	HasSucceeded bool
}

// EdgeStates summarises every route in the mesh over the last window, which is
// the one query the dashboard's matrix is drawn from.
//
// It reads samples, not aggregates. The matrix is a live view and an aggregate
// is by construction minutes behind; a cell that went red a minute ago has to
// be red now, not at the next seal.
//
// Edges touching a node the mesh has removed are left out: the matrix is drawn
// from the servers that are listed, and an edge to one that is not would be a
// cell with nowhere to go.
func (s *Store) EdgeStates(since model.Time) ([]EdgeState, error) {
	rows, err := s.db.Query(`
		SELECT f."Uid", t."Uid", s."Timestamp", s."Value", s."IsOk"
		FROM "Samples" s
		JOIN "Servers" f ON f."Id"=s."Server1"
		JOIN "Servers" t ON t."Id"=s."Server2"
		WHERE s."Param"=? AND s."Timestamp">=? AND f."Removed"=0 AND t."Removed"=0
		ORDER BY s."Timestamp"`, int64(series.LatencyMs), since.DB())
	if err != nil {
		return nil, fmt.Errorf("edge states: %w", err)
	}
	defer rows.Close()

	type edge struct{ from, to string }
	byEdge := map[edge]*EdgeState{}
	order := []edge{}

	for rows.Next() {
		var (
			from, to string
			at       int64
			value    sql.NullInt64
			ok       bool
		)
		if err := rows.Scan(&from, &to, &at, &value, &ok); err != nil {
			return nil, fmt.Errorf("scan edge state: %w", err)
		}

		key := edge{from, to}
		state, seen := byEdge[key]
		if !seen {
			state = &EdgeState{FromUID: from, ToUID: to}
			byEdge[key] = state
			order = append(order, key)
		}

		state.Checks++
		if ok {
			state.Successes++
			state.HasSucceeded = true
		}
		// Rows arrive oldest first, so the last assignment wins and the
		// newest check is what the cell ends up showing.
		state.LastAttempt = model.FromDB(at)
		state.LastCheck = model.FromDB(at)
		state.LastWasUp = ok
		// Left as it was when the newest check produced no number, so a cell
		// goes on showing the last latency actually measured rather than
		// blanking the moment one probe fails.
		if value.Valid {
			ms := series.Decode(series.LatencyMs, value.Int64)
			state.LastLatency = &ms
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]EdgeState, 0, len(order))
	for _, key := range order {
		out = append(out, *byEdge[key])
	}
	return out, nil
}

// LatestEdge reports the newest check on one route. The second bool is false
// when the route has never been measured, which is not the same as having been
// measured as down.
func (s *Store) LatestEdge(fromServerID, toServerID int64) (available, found bool, err error) {
	err = s.db.QueryRow(`
		SELECT "IsOk" FROM "Samples"
		WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0
		ORDER BY "Timestamp" DESC LIMIT 1`,
		fromServerID, int64(series.LatencyMs), toServerID).Scan(&available)
	if errors.Is(err, sql.ErrNoRows) {
		return s.latestEdgeFromRollups(fromServerID, toServerID)
	}
	if err != nil {
		return false, false, fmt.Errorf("latest edge %d->%d: %w", fromServerID, toServerID, err)
	}
	return available, true, nil
}

// latestEdgeFromRollups answers the same question for a route that has not
// been checked within the samples' twenty minutes - a node that has been down
// since before this one started, or a peer whose pushes stopped.
func (s *Store) latestEdgeFromRollups(fromServerID, toServerID int64) (available, found bool, err error) {
	for _, rung := range series.Ladder {
		var okCount int64
		err = s.db.QueryRow(`
			SELECT "CountOk" FROM "Rollups"
			WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0 AND "Level"=?
			ORDER BY "Bucket" DESC LIMIT 1`,
			fromServerID, int64(series.LatencyMs), toServerID, int64(rung.Level)).Scan(&okCount)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, false, fmt.Errorf("latest edge rollup %d->%d: %w", fromServerID, toServerID, err)
		}
		return okCount > 0, true, nil
	}
	return false, false, nil
}

// ── Assembling a Metric ─────────────────────────────────────────────────────

// assembler collects individual parameter readings back into the whole-machine
// samples the dashboard is written against.
//
// The store is one row per value; a Metric is one struct per instant with the
// drives hanging off it. Nothing upstream needs to know that, so the shape is
// restored here rather than spreading the long form through the API and the
// page.
type assembler struct {
	at    []int64
	seen  map[int64]*model.Metric
	disks map[int64]map[string]*model.Disk
}

func newAssembler() *assembler {
	return &assembler{seen: map[int64]*model.Metric{}, disks: map[int64]map[string]*model.Disk{}}
}

// put files one decoded reading under the instant it belongs to.
func (a *assembler) put(at int64, param series.Param, volume string, v float64) {
	m, ok := a.seen[at]
	if !ok {
		m = &model.Metric{}
		a.seen[at] = m
		a.at = append(a.at, at)
	}

	spec, known := series.Of(param)
	if !known {
		// Written by a node running a newer build. Storing what is not
		// understood and ignoring it here is what lets a mesh be upgraded one
		// node at a time.
		return
	}

	if spec.Kind == series.Disk {
		byName, ok := a.disks[at]
		if !ok {
			byName = map[string]*model.Disk{}
			a.disks[at] = byName
		}
		d, ok := byName[volume]
		if !ok {
			d = &model.Disk{Name: volume}
			byName[volume] = d
		}
		switch param {
		case series.DiskTotalGb:
			d.TotalGb = v
		case series.DiskUsedGb:
			d.UsedGb = v
		case series.DiskTempC:
			t := v
			d.TempC = &t
		}
		return
	}

	switch param {
	case series.CPUPercent:
		m.CpuPercent = v
	case series.CPUTempC:
		t := v
		m.CpuTempC = &t
	case series.MemoryPercent:
		m.MemoryPercent = v
	case series.MemoryTotalMb:
		m.MemoryTotalMb = v
	case series.MemoryUsedMb:
		m.MemoryUsedMb = v
	case series.UptimeSeconds:
		m.UptimeSeconds = v
	}
}

// metrics renders what was collected, oldest first, stamping each instant with
// whatever the caller's units mean in time.
func (a *assembler) metrics(stamp func(int64) model.Time) []model.Metric {
	sort.Slice(a.at, func(i, j int) bool { return a.at[i] < a.at[j] })

	out := make([]model.Metric, 0, len(a.at))
	for _, at := range a.at {
		m := *a.seen[at]
		m.Timestamp = stamp(at)

		names := make([]string, 0, len(a.disks[at]))
		for name := range a.disks[at] {
			names = append(names, name)
		}
		// Ordered by name, so a chart's series keep their colours between
		// polls however the rows happened to come back.
		sort.Strings(names)
		for _, name := range names {
			d := *a.disks[at][name]
			// Free space and the percentage are not stored: they are the other
			// two numbers, and keeping them would be keeping the same fact
			// three times in every row.
			d.FreeGb = d.TotalGb - d.UsedGb
			if d.TotalGb > 0 {
				d.UsagePercent = d.UsedGb / d.TotalGb * 100
			}
			m.Disks = append(m.Disks, d)
		}
		out = append(out, m)
	}
	return out
}

// ── How long it has stood this way ──────────────────────────────────────────

// EdgeStreak reports how one route currently stands and when it came to stand
// that way: available is the state of the newest check, and since is when the
// unbroken run of that state began. found is false for a route never measured.
//
// The answer has to cross the boundary between the two tables, and it is worth
// different amounts of precision on each side of it. An outage minutes old is
// settled entirely within the checks themselves, and that is the one the alert
// thresholds are measured against, so it is answered to the millisecond. One
// an hour old has left those behind and survives only as buckets with no
// successful check in them; one a month old only as daily rows. Those are
// answered to the end of the last bucket that disagreed, which is as close as
// an aggregate can come and closer than anybody reading "down for six weeks"
// needs.
//
// Where nothing disagrees anywhere, the run began before any surviving record,
// and the oldest one held is reported instead. The duration is then a floor
// rather than the truth, which for an outage already older than a year is a
// distinction without a difference.
func (s *Store) EdgeStreak(fromServerID, toServerID int64) (available bool, since model.Time, found bool, err error) {
	available, found, err = s.LatestEdge(fromServerID, toServerID)
	if err != nil || !found {
		return false, model.Time{}, false, err
	}

	// The precise answer, while the checks themselves are still held: the
	// oldest check of the current state that is newer than the last one that
	// disagreed. That is the instant the run is measured from, and it is the
	// one the alert thresholds are compared against - which is why it is worth
	// answering exactly for the minutes that matter and approximately for the
	// months that do not.
	var broke sql.NullInt64
	err = s.db.QueryRow(`
		SELECT MAX("Timestamp") FROM "Samples"
		WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0 AND "IsOk"<>?`,
		fromServerID, int64(series.LatencyMs), toServerID, available).Scan(&broke)
	if err != nil {
		return false, model.Time{}, false, fmt.Errorf("streak samples %d->%d: %w", fromServerID, toServerID, err)
	}
	if broke.Valid {
		var began sql.NullInt64
		err = s.db.QueryRow(`
			SELECT MIN("Timestamp") FROM "Samples"
			WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0
				AND "IsOk"=? AND "Timestamp">?`,
			fromServerID, int64(series.LatencyMs), toServerID, available, broke.Int64).Scan(&began)
		if err != nil {
			return false, model.Time{}, false, fmt.Errorf("streak start %d->%d: %w", fromServerID, toServerID, err)
		}
		if began.Valid {
			return available, model.FromDB(began.Int64), true, nil
		}
	}

	// The change is older than the checks that survive individually, so it is
	// looked for in the buckets, where a run is broken by a counter rather than
	// by a row: a run of successes by any failure in a bucket, a run of
	// failures by any success. That is what lets an outage be traced long after
	// the checks themselves were pruned - and the answer is the end of the last
	// bucket that disagreed, which is as close as an aggregate can pin it.
	//
	// The finest level that still reaches back that far gives the closest
	// answer, so every level is asked and the latest reply wins.
	newest := int64(0)
	seen := false
	note := func(ms int64) {
		if !seen || ms > newest {
			newest, seen = ms, true
		}
	}

	disagrees := `"CountOk">0`
	if available {
		disagrees = `"CountOk"<"Count"`
	}
	for _, rung := range series.Ladder {
		var bucket sql.NullInt64
		err = s.db.QueryRow(`
			SELECT MAX("Bucket") FROM "Rollups"
			WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0 AND "Level"=?
				AND `+disagrees,
			fromServerID, int64(series.LatencyMs), toServerID, int64(rung.Level)).Scan(&bucket)
		if err != nil {
			return false, model.Time{}, false, fmt.Errorf("streak rollups %d->%d: %w", fromServerID, toServerID, err)
		}
		if bucket.Valid {
			note(series.BucketEnd(bucket.Int64, rung.Level))
		}
	}

	if seen {
		return available, model.FromDB(newest), true, nil
	}

	// Nothing ever disagreed, so the run reaches back as far as this node's
	// memory of the route goes.
	oldest, ok, err := s.oldestEdgeRecord(fromServerID, toServerID)
	if err != nil {
		return false, model.Time{}, false, err
	}
	if !ok {
		return available, model.Now(), true, nil
	}
	return available, model.FromDB(oldest), true, nil
}

// oldestEdgeRecord is the earliest instant this node holds any record of a
// route at, whichever table it survives in.
func (s *Store) oldestEdgeRecord(fromServerID, toServerID int64) (int64, bool, error) {
	oldest := int64(0)
	seen := false
	note := func(ms int64) {
		if !seen || ms < oldest {
			oldest, seen = ms, true
		}
	}

	var raw sql.NullInt64
	err := s.db.QueryRow(`
		SELECT MIN("Timestamp") FROM "Samples"
		WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0`,
		fromServerID, int64(series.LatencyMs), toServerID).Scan(&raw)
	if err != nil {
		return 0, false, fmt.Errorf("oldest edge sample: %w", err)
	}
	if raw.Valid {
		note(raw.Int64)
	}

	for _, rung := range series.Ladder {
		var bucket sql.NullInt64
		err := s.db.QueryRow(`
			SELECT MIN("Bucket") FROM "Rollups"
			WHERE "Server1"=? AND "Param"=? AND "Server2"=? AND "Volume"=0 AND "Level"=?`,
			fromServerID, int64(series.LatencyMs), toServerID, int64(rung.Level)).Scan(&bucket)
		if err != nil {
			return 0, false, fmt.Errorf("oldest edge rollup: %w", err)
		}
		if bucket.Valid {
			note(series.BucketStart(bucket.Int64, rung.Level))
		}
	}
	return oldest, seen, nil
}

// MaxSampleTimestamp is the newest reading of one parameter held for a server.
//
// It is the high-water mark a peer's resend is filtered against. Peers offer
// an overlapping window every round, and while writing a reading twice is
// harmless - the row is keyed by what it measures and when - not writing it at
// all is cheaper.
//
// Per parameter rather than per server, because the streams a peer sends do
// not advance together: a payload's readings about the machine and its checks
// on its neighbours are gathered at different cadences, and one mark for both
// would silently discard whichever was behind.
func (s *Store) MaxSampleTimestamp(serverID int64, param series.Param) (model.Time, bool, error) {
	ms, ok, err := s.scalar(`SELECT MAX("Timestamp") FROM "Samples" WHERE "Server1"=? AND "Param"=?`,
		"max sample timestamp", serverID, int64(param))
	if err != nil || !ok {
		return model.Time{}, false, err
	}
	return model.FromDB(ms), true, nil
}
