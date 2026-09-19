// Package config loads appsettings.json and resolves the persistent server id.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Options mirrors the "Monitor" section of appsettings.json. Field names are
// matched case-insensitively by encoding/json, so the existing PascalCase file
// is read unchanged.
type Options struct {
	ServerID              string `json:"serverId"`
	ServerName            string `json:"serverName"`
	Location              string `json:"location"`
	PublicURL             string `json:"publicUrl"`
	PushOnly              bool   `json:"pushOnly"`
	ListenPort            int    `json:"listenPort"`
	ListenAddress         string `json:"listenAddress"`
	SharedSecret          string `json:"sharedSecret"`
	DashboardPassword     string `json:"dashboardPassword"`
	DatabasePath          string `json:"databasePath"`
	LibreHardwareMonitor  string `json:"libreHardwareMonitorUrl"`
	MetricIntervalSeconds int    `json:"metricIntervalSeconds"`
	SyncIntervalSeconds   int    `json:"syncIntervalSeconds"`
	RetentionDays         int    `json:"retentionDays"`

	Telegram Telegram `json:"telegram"`
}

// Telegram configures outbound alerts. A node raises them only when both a bot
// token and a chat id are present, so leaving the section out - which every
// node does by default - is how a node opts out.
//
// More than one node in a mesh may be configured to alert, and should be: the
// one node that alerts is also the one node whose own failure nobody reports.
// They do not send duplicates. Each records what it announced, that record
// reaches the others over the ordinary sync, and a node that finds its message
// already sent stays quiet. StaggerSeconds is what keeps two of them from
// deciding at the same instant, before either has heard from the other.
type Telegram struct {
	BotToken         string `json:"botToken"`
	ChatID           string `json:"chatId"`
	DownAfterSeconds int    `json:"downAfterSeconds"`
	RepeatMinutes    int    `json:"repeatMinutes"`
	StaggerSeconds   int    `json:"staggerSeconds"`
}

// Enabled reports whether this node has enough configuration to send anything.
func (t Telegram) Enabled() bool {
	return strings.TrimSpace(t.BotToken) != "" && strings.TrimSpace(t.ChatID) != ""
}

type file struct {
	Monitor Options `json:"monitor"`
}

// maxRetentionDays caps how much history a node keeps, whatever the
// configuration file asks for. Sampling is frequent enough that a longer
// window is mostly disk, and seven days is already the widest range the
// dashboard can plot.
const maxRetentionDays = 7

// Defaults returns the built-in configuration.
func Defaults() Options {
	return Options{
		ServerName:            "My Server",
		Location:              "Unknown",
		ListenPort:            5001,
		ListenAddress:         "0.0.0.0",
		DatabasePath:          "monitor.db",
		MetricIntervalSeconds: 5,
		SyncIntervalSeconds:   10,
		RetentionDays:         maxRetentionDays,
		Telegram: Telegram{
			// Two minutes is long enough that a sync round lost to a reboot,
			// a routing hiccup or a service restart passes without a message,
			// and short enough to still be news.
			DownAfterSeconds: 120,
			RepeatMinutes:    60,
			StaggerSeconds:   60,
		},
	}
}

// BaseDir is the directory holding the executable. Relative paths in the
// configuration resolve against it, so the service behaves the same whatever
// working directory the init system hands it.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// Load reads appsettings.json from path, falling back to defaults for any key
// the file omits. A missing file is not an error.
func Load(path string) (Options, error) {
	opts := Defaults()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return opts, nil
		}
		return opts, fmt.Errorf("read %s: %w", path, err)
	}

	parsed := file{Monitor: opts}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return opts, fmt.Errorf("parse %s: %w", path, err)
	}
	opts = parsed.Monitor

	if opts.ListenPort <= 0 {
		opts.ListenPort = 5001
	}
	if opts.ListenAddress == "" {
		opts.ListenAddress = "0.0.0.0"
	}
	if opts.DatabasePath == "" {
		opts.DatabasePath = "monitor.db"
	}
	if opts.MetricIntervalSeconds <= 0 {
		opts.MetricIntervalSeconds = 5
	}
	if opts.SyncIntervalSeconds <= 0 {
		opts.SyncIntervalSeconds = 10
	}
	// A missing, zero or oversized value all resolve to the cap rather than to
	// unlimited history, so no node can hold data older than the cap allows.
	if opts.RetentionDays <= 0 || opts.RetentionDays > maxRetentionDays {
		opts.RetentionDays = maxRetentionDays
	}

	opts.Telegram.BotToken = strings.TrimSpace(opts.Telegram.BotToken)
	opts.Telegram.ChatID = strings.TrimSpace(opts.Telegram.ChatID)
	if opts.Telegram.DownAfterSeconds <= 0 {
		opts.Telegram.DownAfterSeconds = 120
	}
	if opts.Telegram.RepeatMinutes <= 0 {
		opts.Telegram.RepeatMinutes = 60
	}
	// Zero is a meaningful setting here - a single alerting node needs no
	// stagger at all - so only a negative value is corrected.
	if opts.Telegram.StaggerSeconds < 0 {
		opts.Telegram.StaggerSeconds = 0
	}
	return opts, nil
}

// ResolveServerID fills in ServerId from server-id.txt, generating and
// persisting a new one on first run, so that the identity survives edits to
// appsettings.json.
func ResolveServerID(opts *Options, baseDir string) error {
	if strings.TrimSpace(opts.ServerID) != "" {
		opts.ServerID = strings.TrimSpace(opts.ServerID)
		return nil
	}

	idFile := filepath.Join(baseDir, "server-id.txt")
	if raw, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			opts.ServerID = id
			return nil
		}
	}

	id, err := newGUID()
	if err != nil {
		return err
	}
	if err := os.WriteFile(idFile, []byte(id), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", idFile, err)
	}
	opts.ServerID = id
	return nil
}

// ResolveDatabasePath makes DatabasePath absolute relative to baseDir.
func ResolveDatabasePath(opts Options, baseDir string) string {
	if filepath.IsAbs(opts.DatabasePath) {
		return opts.DatabasePath
	}
	return filepath.Join(baseDir, opts.DatabasePath)
}

// newGUID formats 16 random bytes as an RFC 4122 version 4 UUID, matching the
// server ids written by the previous .NET implementation.
func newGUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate server id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
