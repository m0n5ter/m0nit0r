// Package model holds the wire and storage types shared across the app.
package model

import (
	"fmt"
	"strings"
	"time"
)

// jsonLayout matches how .NET's System.Text.Json writes a DateTime with
// Kind=Unspecified: ISO 8601, no zone suffix, trailing zeros trimmed off the
// fraction. The dashboard appends "Z" itself when parsing, so emitting an
// offset here would produce an invalid date string in the browser.
const jsonLayout = "2006-01-02T15:04:05.9999999"

// Time is a UTC timestamp that round-trips through the .NET-compatible JSON
// form on the wire and a Unix millisecond integer in the database.
type Time struct {
	time.Time
}

// Now returns the current UTC time.
func Now() Time { return At(time.Now()) }

// At wraps t, normalising it to UTC and to the resolution the database keeps.
//
// Truncating here rather than only in DB is what makes a timestamp compare
// equal to itself after a round trip through storage. Peers resend an
// overlapping window every sync round and the receiver discards what is not
// newer than its high-water mark, so a value that came back from SQLite a
// fraction of a millisecond behind the one that was sent would read as newer
// on every resend, and each round would store the same rows again.
func At(t time.Time) Time { return Time{t.UTC().Truncate(time.Millisecond)} }

// MarshalJSON renders the timestamp the way the dashboard expects it.
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.UTC().Format(jsonLayout) + `"`), nil
}

// UnmarshalJSON accepts both the zone-less .NET form and ordinary RFC 3339, so
// that peers running either implementation can be read. Whatever resolution
// arrives is cut to the one this side stores, for the same reason At does it.
func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	// A layout without a fractional part still parses input that has one.
	for _, layout := range []string{"2006-01-02T15:04:05", time.RFC3339Nano} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = At(v)
			return nil
		}
	}
	return fmt.Errorf("model: cannot parse time %q", s)
}

// DB renders the timestamp for storage in SQLite, as milliseconds since the
// Unix epoch. An integer sorts, ranges and buckets arithmetically, where the
// TEXT encoding this replaced only sorted chronologically because every value
// was padded to the same width, and had to go through strftime to be grouped.
// Millisecond resolution is far finer than the sampling interval.
func (t Time) DB() int64 { return t.UTC().UnixMilli() }

// FromDB reads a timestamp back out of SQLite. The zero Time survives the round
// trip, so a row that was never stamped still reads as unset.
func FromDB(ms int64) Time { return Time{time.UnixMilli(ms).UTC()} }

// Disk is one fixed volume in a snapshot. TempC is the temperature of the drive
// backing the volume, nil when the drive exposes no sensor.
type Disk struct {
	Name         string   `json:"name"`
	TotalGb      float64  `json:"totalGb"`
	UsedGb       float64  `json:"usedGb"`
	FreeGb       float64  `json:"freeGb"`
	UsagePercent float64  `json:"usagePercent"`
	TempC        *float64 `json:"tempC"`
}

// Snapshot is one sample of local system state. Temperatures are pointers
// because a host with no readable sensor has to be distinguishable from one
// genuinely sitting at zero degrees.
type Snapshot struct {
	CpuPercent    float64  `json:"cpuPercent"`
	CpuTempC      *float64 `json:"cpuTempC"`
	MemoryPercent float64  `json:"memoryPercent"`
	MemoryTotalMb float64  `json:"memoryTotalMb"`
	MemoryUsedMb  float64  `json:"memoryUsedMb"`
	UptimeSeconds float64  `json:"uptimeSeconds"`
	Disks         []Disk   `json:"disks"`
}

// Server is a node known to this instance, self included.
//
// ID is the row id in this node's own database and means nothing anywhere
// else; UID is the identity the node was given at first start and the one that
// travels, so it is what the peer protocol and the dashboard both speak. The
// tables reference servers by ID, which is why the two are carried together.
type Server struct {
	ID       int64
	UID      string
	Name     string
	Location string
	URL      string
	IsSelf   bool
	Alerts   bool
	LastSeen Time
}

// Metric is a stored sample, for either this server or a peer. The volumes are
// a slice rather than columns of their own because the set of drives varies
// per host and changes under it; they are stored one row per volume, in
// MetricDisks.
type Metric struct {
	Timestamp     Time     `json:"timestamp"`
	CpuPercent    float64  `json:"cpuPercent"`
	CpuTempC      *float64 `json:"cpuTempC"`
	MemoryPercent float64  `json:"memoryPercent"`
	MemoryTotalMb float64  `json:"memoryTotalMb"`
	MemoryUsedMb  float64  `json:"memoryUsedMb"`
	UptimeSeconds float64  `json:"uptimeSeconds"`
	Disks         []Disk   `json:"disks"`
}

// Availability is one reachability observation from one server to another.
type Availability struct {
	ToServerID  string   `json:"toServerId"`
	Timestamp   Time     `json:"timestamp"`
	IsAvailable bool     `json:"isAvailable"`
	LatencyMs   *float64 `json:"latencyMs"`
	HTTPStatus  *int     `json:"httpStatus"`
}

// Notice records that some node announced a change in another node's
// reachability - to Telegram, today - so that the rest of the mesh can see the
// announcement was already made and stay quiet.
//
// It carries no message text. What was said is reconstructable from the
// availability records that travel alongside it; what the other nodes need is
// only that somebody said it, about whom, and when.
type Notice struct {
	ToServerID string `json:"toServerId"`
	Timestamp  Time   `json:"timestamp"`
	IsDown     bool   `json:"isDown"`
}

// SyncPayload is the body of POST /api/sync.
type SyncPayload struct {
	ServerID     string         `json:"serverId"`
	ServerName   string         `json:"serverName"`
	Location     string         `json:"location"`
	SelfURL      string         `json:"selfUrl"`
	Alerts       bool           `json:"alerts"`
	Metrics      []Metric       `json:"metrics"`
	Availability []Availability `json:"availability"`
	Notices      []Notice       `json:"notices"`
}

// IntroduceRequest is the body of POST /api/introduce.
type IntroduceRequest struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	Location   string `json:"location"`
	SelfURL    string `json:"selfUrl"`
	Alerts     bool   `json:"alerts"`
}

// HealthResponse identifies this instance to a caller.
type HealthResponse struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	Location   string `json:"location"`
	Alerts     bool   `json:"alerts"`
	Timestamp  Time   `json:"timestamp"`
}
