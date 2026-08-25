// Command monitor is the m0nit0r agent: it samples the local host, exchanges
// data with peer instances, and serves the dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/api"
	"github.com/m0n5ter/m0nit0r/internal/auth"
	"github.com/m0n5ter/m0nit0r/internal/config"
	"github.com/m0n5ter/m0nit0r/internal/metrics"
	"github.com/m0n5ter/m0nit0r/internal/peer"
	"github.com/m0n5ter/m0nit0r/internal/store"
	"github.com/m0n5ter/m0nit0r/internal/worker"
)

// shutdownGrace is how long in-flight requests get to finish on stop.
const shutdownGrace = 10 * time.Second

// maxLogBytes is the size at which the service-mode log file is rotated once,
// so an unattended service cannot fill the disk with its own logs.
const maxLogBytes = 10 << 20

func main() {
	configPath := flag.String("config", "", "path to appsettings.json (default: beside the executable)")
	verbose := flag.Bool("v", false, "log at debug level")
	flag.Parse()

	baseDir := config.BaseDir()
	if *configPath == "" {
		*configPath = filepath.Join(baseDir, "appsettings.json")
	}

	service := inServiceMode()
	log, closeLog, err := newLogger(baseDir, service, *verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open log:", err)
		os.Exit(1)
	}
	defer closeLog()

	opts, err := config.Load(*configPath)
	if err != nil {
		log.Error("load configuration", "path", *configPath, "err", err)
		os.Exit(1)
	}
	if err := config.ResolveServerID(&opts, baseDir); err != nil {
		log.Error("resolve server id", "err", err)
		os.Exit(1)
	}

	run := func(ctx context.Context) error { return runApp(ctx, log, opts, baseDir) }

	if service {
		err = runService(run)
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = run(ctx)
	}

	if err != nil {
		log.Error("shutting down after failure", "err", err)
		closeLog()
		os.Exit(1)
	}
}

// runApp brings up storage, the background workers and the HTTP server, then
// blocks until ctx is cancelled or the listener fails.
func runApp(parent context.Context, log *slog.Logger, opts config.Options, baseDir string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	dbPath := config.ResolveDatabasePath(opts, baseDir)
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.UpsertSelf(opts.ServerID, opts.ServerName, opts.Location,
		peer.NormalizeURL(opts.PublicURL)); err != nil {
		return err
	}

	log.Info("m0nit0r starting",
		"serverId", opts.ServerID,
		"name", opts.ServerName,
		"location", opts.Location,
		"database", dbPath)

	signer := auth.New(opts.SharedSecret)
	if !signer.Enabled() {
		log.Warn("SharedSecret is not set: /api/sync and /api/introduce accept writes from anyone who can reach this port")
	}

	client := peer.New(opts.SharedSecret)

	apiServer := &api.Server{
		Store:      st,
		Client:     client,
		Signer:     signer,
		ServerID:   opts.ServerID,
		ServerName: opts.ServerName,
		Location:   opts.Location,
		PublicURL:  peer.NormalizeURL(opts.PublicURL),
		Log:        log,
	}

	httpServer := &http.Server{
		Addr:              net.JoinHostPort(opts.ListenAddress, strconv.Itoa(opts.ListenPort)),
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind before announcing readiness so that a port clash is reported as a
	// start-up failure rather than as a service that came up and did nothing.
	listener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", httpServer.Addr, err)
	}

	workers := []func(context.Context){
		(&worker.Metrics{
			Store: st,
			Collector: metrics.New(metrics.Options{
				LibreHardwareMonitorURL: opts.LibreHardwareMonitor,
				Log:                     log,
			}),
			ServerID:  opts.ServerID,
			Interval:  time.Duration(opts.MetricIntervalSeconds) * time.Second,
			Retention: time.Duration(opts.RetentionDays) * 24 * time.Hour,
			Log:       log,
		}).Run,
		(&worker.Sync{
			Store:      st,
			Client:     client,
			ServerID:   opts.ServerID,
			ServerName: opts.ServerName,
			Location:   opts.Location,
			PublicURL:  peer.NormalizeURL(opts.PublicURL),
			Interval:   time.Duration(opts.SyncIntervalSeconds) * time.Second,
			Log:        log,
		}).Run,
	}

	var wg sync.WaitGroup
	for _, run := range workers {
		wg.Add(1)
		go func(run func(context.Context)) {
			defer wg.Done()
			run(ctx)
		}(run)
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	log.Info("dashboard listening", "addr", httpServer.Addr)
	notifyReady()

	var runErr error
	select {
	case runErr = <-serveErr:
	case <-ctx.Done():
	}

	cancel()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}

	wg.Wait()
	log.Info("m0nit0r stopped")
	return runErr
}

// newLogger writes to stderr when run interactively. Under a service manager
// that discards stdio there would be nowhere to look after a failure, so it
// writes to a file beside the executable instead.
func newLogger(baseDir string, service, verbose bool) (*slog.Logger, func(), error) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	if !service {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), func() {}, nil
	}

	path := filepath.Join(baseDir, "monitor.log")
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		os.Rename(path, path+".old")
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}

	var closeOnce sync.Once
	return slog.New(slog.NewTextHandler(io.Writer(f), opts)), func() { closeOnce.Do(func() { f.Close() }) }, nil
}
