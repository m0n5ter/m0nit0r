package store

import (
	"database/sql"
	"fmt"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/series"
)

// This file turns rows into the wire form and back. Two translations happen
// here and nowhere else: a row id means nothing outside the database that
// issued it, so the far end of an edge travels as the identity its node
// publishes, and a drive travels as its name.

// OwnSamplesSince returns this node's own readings from after since, oldest
// first, in the form a peer is sent them.
//
// Only a node's own readings are its to forward. Relaying what a peer reported
// would put the same reading on the wire once per hop, and every node in a
// mesh already hears from every other.
func (s *Store) OwnSamplesSince(serverID int64, since model.Time, limit int) ([]model.SampleRow, model.Time, error) {
	rows, err := s.db.Query(`
		SELECT s."Param", COALESCE(t."Uid",''), COALESCE(v."Name",''), s."Timestamp", s."Value", s."IsOk"
		FROM "Samples" s
		LEFT JOIN "Servers" t ON t."Id"=s."Server2"
		LEFT JOIN "Volumes" v ON v."Id"=s."Volume"
		WHERE s."Server1"=? AND s."Timestamp">?
		ORDER BY s."Timestamp"
		LIMIT ?`, serverID, since.DB(), limit)
	if err != nil {
		return nil, since, fmt.Errorf("own samples since: %w", err)
	}
	defer rows.Close()

	out := []model.SampleRow{}
	high := since
	for rows.Next() {
		var (
			param int64
			at    int64
			value sql.NullInt64
			r     model.SampleRow
		)
		if err := rows.Scan(&param, &r.Peer, &r.Volume, &at, &value, &r.Ok); err != nil {
			return nil, since, fmt.Errorf("scan own sample: %w", err)
		}
		r.Param = uint8(param)
		r.Timestamp = model.FromDB(at)
		if value.Valid {
			v := value.Int64
			r.Value = &v
		}
		if r.Timestamp.After(high.Time) {
			high = r.Timestamp
		}
		out = append(out, r)
	}
	return out, high, rows.Err()
}

// OwnRollups returns this node's own sealed buckets at one level, after a
// watermark and no later than the newest that can no longer change.
//
// The limit is what keeps a node meeting a peer for the first time from
// putting its whole history into one request. The high-water mark comes back
// alongside, so a caller that was cut short resumes exactly where the rows
// stopped and the rest follows over the next few rounds.
func (s *Store) OwnRollups(serverID int64, level series.Level, after, through int64, limit int) ([]model.RollupRow, int64, error) {
	rows, err := s.db.Query(`
		SELECT r."Param", COALESCE(t."Uid",''), COALESCE(v."Name",''), r."Bucket",
			r."Mn", r."Mx", r."Sum", r."Count", r."CountOk", r."CountVal"
		FROM "Rollups" r
		LEFT JOIN "Servers" t ON t."Id"=r."Server2"
		LEFT JOIN "Volumes" v ON v."Id"=r."Volume"
		WHERE r."Server1"=? AND r."Level"=? AND r."Bucket">? AND r."Bucket"<=?
		ORDER BY r."Bucket"
		LIMIT ?`, serverID, int64(level), after, through, limit)
	if err != nil {
		return nil, after, fmt.Errorf("own rollups at level %d: %w", level, err)
	}
	defer rows.Close()

	out := []model.RollupRow{}
	high := after
	for rows.Next() {
		var (
			param  int64
			mn, mx sql.NullInt64
			r      model.RollupRow
		)
		if err := rows.Scan(&param, &r.Peer, &r.Volume, &r.Bucket,
			&mn, &mx, &r.Sum, &r.Count, &r.CountOk, &r.CountVal); err != nil {
			return nil, after, fmt.Errorf("scan own rollup: %w", err)
		}
		r.Param, r.Level = uint8(param), uint8(level)
		if mn.Valid {
			v := mn.Int64
			r.Mn = &v
		}
		if mx.Valid {
			v := mx.Int64
			r.Mx = &v
		}
		if r.Bucket > high {
			high = r.Bucket
		}
		out = append(out, r)
	}
	return out, high, rows.Err()
}

// OldestOwnBucket names the earliest bucket this node holds of its own making
// at a level, which is where a peer that has never heard from it is started
// from so that it receives the history rather than only what happens next.
func (s *Store) OldestOwnBucket(serverID int64, level series.Level) (int64, bool, error) {
	return s.scalar(`SELECT MIN("Bucket") FROM "Rollups" WHERE "Server1"=? AND "Level"=?`,
		"oldest own bucket", serverID, int64(level))
}

// ── Taking them in ──────────────────────────────────────────────────────────

// StoreSamples writes the readings a peer reported, resolving the identities
// they name to local rows.
//
// The subjects are the nodes the peer can see, which is not always the set
// this one knows, so an unfamiliar id is registered rather than dropped - the
// same rule the first build followed, and what keeps a reading about a node
// this one has not been introduced to yet.
func (s *Store) StoreSamples(fromServerID int64, rows []model.SampleRow) error {
	if len(rows) == 0 {
		return nil
	}

	peers := map[string]int64{}
	volumes := map[string]int64{}
	out := make([]Sample, 0, len(rows))

	for _, r := range rows {
		smp := Sample{
			Param:     series.Param(r.Param),
			Server1:   fromServerID,
			Timestamp: r.Timestamp,
			Value:     r.Value,
			Ok:        r.Ok,
		}

		if r.Peer != "" {
			id, err := resolve(peers, r.Peer, s.EnsureServer)
			if err != nil {
				return err
			}
			smp.Server2 = id
		}
		if r.Volume != "" {
			id, err := resolve(volumes, r.Volume, func(name string) (int64, error) {
				return s.VolumeID(fromServerID, name)
			})
			if err != nil {
				return err
			}
			smp.Volume = id
		}
		out = append(out, smp)
	}

	return s.InsertSamples(out)
}

// StoreRollups does the same for the sealed buckets a peer reported.
func (s *Store) StoreRollups(fromServerID int64, rows []model.RollupRow) error {
	if len(rows) == 0 {
		return nil
	}

	peers := map[string]int64{}
	volumes := map[string]int64{}
	out := make([]Bucket, 0, len(rows))

	for _, r := range rows {
		b := Bucket{
			Param:    series.Param(r.Param),
			Server1:  fromServerID,
			Level:    series.Level(r.Level),
			Bucket:   r.Bucket,
			Mn:       r.Mn,
			Mx:       r.Mx,
			Sum:      r.Sum,
			Count:    r.Count,
			CountOk:  r.CountOk,
			CountVal: r.CountVal,
		}

		if r.Peer != "" {
			id, err := resolve(peers, r.Peer, s.EnsureServer)
			if err != nil {
				return err
			}
			b.Server2 = id
		}
		if r.Volume != "" {
			id, err := resolve(volumes, r.Volume, func(name string) (int64, error) {
				return s.VolumeID(fromServerID, name)
			})
			if err != nil {
				return err
			}
			b.Volume = id
		}
		out = append(out, b)
	}

	return s.UpsertRollups(out)
}

// resolve looks a name up once per payload rather than once per row. A round
// carries thousands of rows naming a handful of peers and drives between them.
func resolve(seen map[string]int64, name string, lookup func(string) (int64, error)) (int64, error) {
	if id, ok := seen[name]; ok {
		return id, nil
	}
	id, err := lookup(name)
	if err != nil {
		return 0, err
	}
	seen[name] = id
	return id, nil
}

// OwnReachabilitySince renders this node's own checks in the original form of
// the payload, for a peer too old to read any other.
//
// It comes out of the series store rather than the table the first build kept
// it in, so that speaking the old protocol outlives that table: what a peer
// understands is a question about the wire, and the storage underneath it is
// nobody else's business.
//
// The HTTP status is not carried. It was only ever stored to be shown in a
// tooltip, it is not part of any aggregate, and the one thing it said that
// mattered - whether the check succeeded - travels in its own field.
func (s *Store) OwnReachabilitySince(serverID int64, since model.Time) ([]model.Availability, error) {
	rows, err := s.db.Query(`
		SELECT t."Uid", s."Timestamp", s."Value", s."IsOk"
		FROM "Samples" s
		JOIN "Servers" t ON t."Id"=s."Server2"
		WHERE s."Server1"=? AND s."Param"=? AND s."Timestamp">=?
		ORDER BY s."Timestamp"`, serverID, int64(series.LatencyMs), since.DB())
	if err != nil {
		return nil, fmt.Errorf("own reachability since: %w", err)
	}
	defer rows.Close()

	out := []model.Availability{}
	for rows.Next() {
		var (
			a     model.Availability
			at    int64
			value sql.NullInt64
		)
		if err := rows.Scan(&a.ToServerID, &at, &value, &a.IsAvailable); err != nil {
			return nil, fmt.Errorf("scan own reachability: %w", err)
		}
		a.Timestamp = model.FromDB(at)
		if value.Valid {
			ms := series.Decode(series.LatencyMs, value.Int64)
			a.LatencyMs = &ms
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
