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
	ListenPort            int    `json:"listenPort"`
	ListenAddress         string `json:"listenAddress"`
	SharedSecret          string `json:"sharedSecret"`
	DatabasePath          string `json:"databasePath"`
	LibreHardwareMonitor  string `json:"libreHardwareMonitorUrl"`
	MetricIntervalSeconds int    `json:"metricIntervalSeconds"`
	SyncIntervalSeconds   int    `json:"syncIntervalSeconds"`
	RetentionDays         int    `json:"retentionDays"`
}

type file struct {
	Monitor Options `json:"monitor"`
}

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
		RetentionDays:         30,
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
