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

// dbLayout matches EF Core's SQLite DateTime encoding: same shape but space
// separated and fixed-width, so that lexicographic TEXT comparison in SQL is
// equivalent to chronological comparison. Widths must not vary here.
const dbLayout = "2006-01-02 15:04:05.0000000"

// Time is a UTC timestamp that round-trips through both the .NET-compatible
// JSON form and the EF Core-compatible SQLite TEXT form.
type Time struct {
	time.Time
}

// Now returns the current UTC time.
func Now() Time { return Time{time.Now().UTC()} }

// At wraps t, normalising it to UTC.
func At(t time.Time) Time { return Time{t.UTC()} }

// MarshalJSON renders the timestamp the way the dashboard expects it.
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.UTC().Format(jsonLayout) + `"`), nil
}

// UnmarshalJSON accepts both the zone-less .NET form and ordinary RFC 3339, so
// that peers running either implementation can be read.
func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	// A layout without a fractional part still parses input that has one.
	for _, layout := range []string{"2006-01-02T15:04:05", time.RFC3339Nano} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
	}
	return fmt.Errorf("model: cannot parse time %q", s)
}

// DB renders the timestamp for storage in SQLite.
func (t Time) DB() string { return t.UTC().Format(dbLayout) }

// ParseDB reads a timestamp back out of SQLite, tolerating rows written with a
// shorter fraction than the canonical seven digits.
func ParseDB(s string) (Time, error) {
	if s == "" {
		return Time{}, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if v, err := time.Parse(layout, s); err == nil {
			return Time{v.UTC()}, nil
		}
	}
	return Time{}, fmt.Errorf("model: cannot parse stored time %q", s)
}

// Disk is one fixed volume in a snapshot.
type Disk struct {
	Name         string  `json:"name"`
	TotalGb      float64 `json:"totalGb"`
	UsedGb       float64 `json:"usedGb"`
	FreeGb       float64 `json:"freeGb"`
	UsagePercent float64 `json:"usagePercent"`
}

// Snapshot is one sample of local system state.
type Snapshot struct {
	CpuPercent    float64 `json:"cpuPercent"`
	MemoryPercent float64 `json:"memoryPercent"`
	MemoryTotalMb float64 `json:"memoryTotalMb"`
	MemoryUsedMb  float64 `json:"memoryUsedMb"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	Disks         []Disk  `json:"disks"`
}

// Server is a node known to this instance, self included.
type Server struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	URL      string `json:"url"`
	IsSelf   bool   `json:"isSelf"`
	LastSeen Time   `json:"lastSeen"`
}

// Metric is a stored sample, for either this server or a peer.
type Metric struct {
	Timestamp     Time    `json:"timestamp"`
	CpuPercent    float64 `json:"cpuPercent"`
	MemoryPercent float64 `json:"memoryPercent"`
	MemoryTotalMb float64 `json:"memoryTotalMb"`
	MemoryUsedMb  float64 `json:"memoryUsedMb"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	DisksJSON     string  `json:"disksJson"`
}

// Availability is one reachability observation from one server to another.
type Availability struct {
	ToServerID  string   `json:"toServerId"`
	Timestamp   Time     `json:"timestamp"`
	IsAvailable bool     `json:"isAvailable"`
	LatencyMs   *float64 `json:"latencyMs"`
	HTTPStatus  *int     `json:"httpStatus"`
}

// SyncPayload is the body of POST /api/sync.
type SyncPayload struct {
	ServerID     string         `json:"serverId"`
	ServerName   string         `json:"serverName"`
	Location     string         `json:"location"`
	SelfURL      string         `json:"selfUrl"`
	Metrics      []Metric       `json:"metrics"`
	Availability []Availability `json:"availability"`
}

// IntroduceRequest is the body of POST /api/introduce.
type IntroduceRequest struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	Location   string `json:"location"`
	SelfURL    string `json:"selfUrl"`
}

// HealthResponse identifies this instance to a caller.
type HealthResponse struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	Location   string `json:"location"`
	Timestamp  Time   `json:"timestamp"`
}
