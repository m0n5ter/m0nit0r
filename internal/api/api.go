// Package api exposes the HTTP surface: the peer protocol, the dashboard's
// JSON endpoints, and the dashboard itself.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/auth"
	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/peer"
	"github.com/m0n5ter/m0nit0r/internal/series"
	"github.com/m0n5ter/m0nit0r/internal/store"
	"github.com/m0n5ter/m0nit0r/internal/web"
)

// maxBodyBytes caps inbound request bodies. Sync payloads from a busy peer are
// the largest legitimate input and stay far below this.
const maxBodyBytes = 8 << 20

// matrixWindow is how far back the availability matrix looks.
//
// It used to be an hour, of which only the last ten checks per edge were used;
// now it is the span those ten checks actually cover, because the rows are read
// per edge rather than filtered afterwards. At a ten-second sync that is a
// dozen checks, which is what the percentage in a cell is out of.
const matrixWindow = 2 * time.Minute

// Server wires the HTTP handlers to storage and the peer client.
//
// ServerID is this node's row id, which is what the store is addressed by;
// ServerUID is the identity it publishes, which is what peers and the dashboard
// use. Both are fixed for the life of the process.
type Server struct {
	Store      *store.Store
	Client     *peer.Client
	Signer     *auth.Signer
	Gate       *auth.Gate
	ServerID   int64
	ServerUID  string
	ServerName string
	Location   string
	PublicURL  string
	Alerts     bool
	PushOnly   bool
	Log        *slog.Logger
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/introduce", s.signed(s.handleIntroduce))
	mux.HandleFunc("POST /api/sync", s.signed(s.handleSync))
	mux.HandleFunc("GET /api/servers", s.handleServers)
	mux.HandleFunc("GET /api/servers/{id}/metrics", s.handleServerMetrics)
	mux.HandleFunc("GET /api/availability/matrix", s.handleAvailabilityMatrix)
	mux.HandleFunc("GET /api/availability/history/{fromId}/{toId}", s.handleAvailabilityHistory)
	mux.HandleFunc("GET /api/peers", s.handleListPeers)
	mux.HandleFunc("POST /api/peers", s.handleAddPeer)
	mux.HandleFunc("DELETE /api/peers/{id}", s.handleRemovePeer)

	mux.Handle("GET /{$}", web.Dashboard())
	mux.Handle("GET /assets/", web.Assets())

	return s.guarded(mux)
}

// openPaths are served without the dashboard password.
//
// The two peer-protocol endpoints carry a shared-secret signature instead, and
// the health check names the node and nothing more - it is what the deployment
// script and the Android app probe an address with before they have any
// credentials. The dashboard page and its assets hold no data of their own; the
// page is what shows the login form, so it has to load before anybody has
// logged in. The session endpoints are how they do.
var openPaths = map[string]bool{
	"/":              true,
	"/api/health":    true,
	"/api/sync":      true,
	"/api/introduce": true,
	"/api/login":     true,
	"/api/logout":    true,
	"/api/session":   true,
}

// guarded puts everything else - every read endpoint and peer management -
// behind the dashboard password.
//
// A refusal carries no WWW-Authenticate challenge. That header is what makes a
// browser raise its own login dialog; the dashboard asks with a form of its
// own instead. Clients that send basic authentication unprompted, as the
// Android app and curl -u do, are unaffected.
func (s *Server) guarded(next http.Handler) http.Handler {
	if s.Gate == nil || !s.Gate.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPaths[r.URL.Path] || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}

		switch result, wait := s.Gate.Check(r); result {
		case auth.Allowed:
			next.ServeHTTP(w, r)
		case auth.LockedOut:
			lockedOut(w, wait)
		default:
			if _, _, supplied := r.BasicAuth(); supplied {
				s.Log.Warn("rejected dashboard login", "path", r.URL.Path, "remote", r.RemoteAddr)
			}
			http.Error(w, "password required", http.StatusUnauthorized)
		}
	})
}

func lockedOut(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	http.Error(w, "too many failed logins; try again later", http.StatusTooManyRequests)
}

// ── Dashboard session ───────────────────────────────────────────────────────

type sessionView struct {
	// PasswordRequired is false on a node without a password, and for a
	// browser on the host itself, which is let through without one.
	PasswordRequired bool `json:"passwordRequired"`
	SignedIn         bool `json:"signedIn"`
}

// handleSession tells the dashboard whether to show its login form and
// whether a sign-out button means anything.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	required := s.Gate != nil && s.Gate.Enabled() && !auth.IsLoopback(r.RemoteAddr)
	signedIn := !required
	if required {
		result, _ := s.Gate.Check(r)
		signedIn = result == auth.Allowed
	}
	writeJSON(w, http.StatusOK, sessionView{PasswordRequired: required, SignedIn: signedIn})
}

// handleLogin trades the password for a session cookie. Wrong guesses count
// towards the same lockout basic authentication does, so the form is no
// easier to guess through.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.Gate == nil || !s.Gate.Enabled() {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch result, wait := s.Gate.Try(r.RemoteAddr, req.Password); result {
	case auth.Allowed:
		http.SetCookie(w, s.Gate.NewSession(req.Remember))
		w.WriteHeader(http.StatusNoContent)
	case auth.LockedOut:
		lockedOut(w, wait)
	default:
		s.Log.Warn("rejected dashboard login", "remote", r.RemoteAddr)
		http.Error(w, "wrong password", http.StatusUnauthorized)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, auth.EndSession())
	w.WriteHeader(http.StatusNoContent)
}

// signed wraps the peer-protocol handlers with shared-secret verification.
//
// The body has to be read here to check the signature over the exact bytes
// received, so it is buffered and handed back to the handler intact.
func (s *Server) signed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		if err := s.Signer.Verify(
			r.Header.Get(auth.HeaderTimestamp),
			r.Header.Get(auth.HeaderSignature),
			r.URL.Path, body, time.Now(),
		); err != nil {
			s.Log.Warn("rejected peer request",
				"path", r.URL.Path, "remote", r.RemoteAddr, "reason", err)
			http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// ── Identity and peer protocol ──────────────────────────────────────────────

func (s *Server) identity() model.HealthResponse {
	return model.HealthResponse{
		ServerID:   s.ServerUID,
		ServerName: s.ServerName,
		Location:   s.Location,
		Alerts:     s.Alerts,
		Timestamp:  model.Now(),
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.identity())
}

func (s *Server) handleIntroduce(w http.ResponseWriter, r *http.Request) {
	var req model.IntroduceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.refuseRemoved(w, req.ServerID) {
		return
	}

	// An introduction without a reachable address tells us nothing worth
	// storing, so record the caller only when both id and URL are present -
	// or when it is a push-only node, which has no address by design and is
	// worth knowing about before its first push arrives.
	if req.ServerID != "" && req.ServerID != s.ServerUID &&
		(peer.NormalizeURL(req.SelfURL) != "" || req.PushOnly) {
		id, err := s.Store.UpsertPeer(req.ServerID, req.ServerName, req.Location,
			peer.NormalizeURL(req.SelfURL), req.Alerts, model.Now())
		if err == nil {
			err = s.Store.SetPushOnly(id, req.PushOnly)
		}
		if err != nil {
			s.Log.Error("record introducing peer", "peer", req.ServerID, "err", err)
			http.Error(w, "failed to record peer", http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusOK, s.identity())
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var payload model.SyncPayload
	if !decodeJSON(w, r, &payload) {
		return
	}
	if payload.ServerID == "" {
		http.Error(w, "ServerId required", http.StatusBadRequest)
		return
	}
	if payload.ServerID == s.ServerUID {
		http.Error(w, "refusing to sync a payload from this server's own id", http.StatusBadRequest)
		return
	}

	// Before anything is stored: a node that was taken out of this mesh must
	// not be able to put itself back into the register simply by going on
	// pushing. The refusal is also what tells it, since nobody can rely on
	// reaching it to say so - see refuseRemoved.
	if s.refuseRemoved(w, payload.ServerID) {
		return
	}

	// Ahead of the peer's own data, so that a removal it is carrying about a
	// third node takes effect in the same request that brings that node's
	// readings - and so that a removal of a node whose id has never been seen
	// here has a row to be written on before the readings make a live one.
	s.applyRemovals(payload)

	peerID, err := s.Store.UpsertPeer(payload.ServerID, payload.ServerName, payload.Location,
		peer.NormalizeURL(payload.SelfURL), payload.Alerts, model.Now())
	if err == nil {
		err = s.Store.SetPushOnly(peerID, payload.PushOnly)
	}
	if err == nil {
		// Remembered before anything is read out of the payload, because it
		// is what decides how the rest of it is read - and what this node
		// will send back in the other direction from the next round on.
		err = s.Store.SetProtocol(peerID, payload.Protocol)
	}
	if err != nil {
		s.Log.Error("record syncing peer", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to record peer", http.StatusInternalServerError)
		return
	}

	if err := s.storeSyncedSeries(peerID, payload); err != nil {
		s.Log.Error("store synced series", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to store series", http.StatusInternalServerError)
		return
	}

	// The original form is read only from a peer that speaks nothing else. A
	// node sending both would otherwise have every reading stored twice, once
	// as it measured it and once as this node re-derived it from the snapshot.
	if payload.Protocol < model.ProtocolVersion {
		if err := s.storeSyncedMetrics(peerID, payload); err != nil {
			s.Log.Error("store synced metrics", "peer", payload.ServerID, "err", err)
			http.Error(w, "failed to store metrics", http.StatusInternalServerError)
			return
		}
		if err := s.storeSyncedAvailability(peerID, payload); err != nil {
			s.Log.Error("store synced availability", "peer", payload.ServerID, "err", err)
			http.Error(w, "failed to store availability", http.StatusInternalServerError)
			return
		}
	}
	if err := s.storeSyncedNotices(peerID, payload); err != nil {
		s.Log.Error("store synced notices", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to store notices", http.StatusInternalServerError)
		return
	}

	s.replyWithPeers(w, payload.ServerID)
}

// refuseRemoved rejects a peer-protocol call from a node this mesh has removed,
// and reports whether it did.
//
// The status is the message. A removed node is not pushed to and not probed, so
// the reply to its own pushes is the only channel left to reach it on, and 410
// is what its sync worker reads as "this peer is not mine any more" and drops
// the peer on. Once the removal has travelled the mesh - which it does in the
// ordinary pushes - every remaining node answers it the same way, and it stops
// syncing with all of them without anybody having to reach it.
func (s *Server) refuseRemoved(w http.ResponseWriter, uid string) bool {
	if uid == "" {
		return false
	}
	removed, err := s.Store.IsRemoved(uid)
	if err != nil {
		s.Log.Error("check removed peer", "peer", uid, "err", err)
		http.Error(w, "failed to check membership", http.StatusInternalServerError)
		return true
	}
	if removed {
		s.Log.Debug("refused removed peer", "peer", uid)
		http.Error(w, "this server was removed from the mesh", http.StatusGone)
		return true
	}
	return false
}

// applyRemovals takes in the membership decisions a peer carried. Failures are
// logged and not returned: the rest of the payload is worth storing either way,
// and the next round brings the same list again.
func (s *Server) applyRemovals(payload model.SyncPayload) {
	for _, r := range payload.Removals {
		// A node is not removable from its own database - it would erase its
		// own history and its place in every matrix - and the decision to
		// remove the peer that is delivering it was evidently reversed.
		if r.ServerID == "" || r.ServerID == s.ServerUID || r.ServerID == payload.ServerID {
			continue
		}
		changed, err := s.Store.SetMembership(r.ServerID, r.Removed, r.Timestamp)
		if err != nil {
			s.Log.Error("apply removal", "peer", r.ServerID, "err", err)
			continue
		}
		if changed {
			s.Log.Info("mesh membership changed", "peer", r.ServerID,
				"removed", r.Removed, "via", payload.ServerName)
		}
	}
}

// replyWithPeers answers a sync with the peers this node pushes to, which is
// how the mesh closes over itself: whoever joins through any one node is
// pushing to all of them within a round or two, and a push-only node - which
// nobody can reach to introduce anything to - learns of the mesh this way and
// no other. The reply is signed, because the sender will start pushing its
// data to whatever addresses it names.
func (s *Server) replyWithPeers(w http.ResponseWriter, callerUID string) {
	peers, err := s.Store.ListPeers()
	if err != nil {
		// The data is already stored; failing the push over the peer list
		// would record an outage that did not happen.
		s.Log.Error("list peers for sync reply", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	reply := model.SyncReply{Peers: make([]model.PeerInfo, 0, len(peers))}
	for _, p := range peers {
		if p.UID == callerUID {
			continue
		}
		reply.Peers = append(reply.Peers, model.PeerInfo{
			ServerID:   p.UID,
			ServerName: p.Name,
			Location:   p.Location,
			URL:        p.URL,
			Alerts:     p.Alerts,
		})
	}

	body, err := json.Marshal(reply)
	if err != nil {
		s.Log.Error("encode sync reply", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.Signer.Enabled() {
		timestamp, signature := s.Signer.Sign(peer.ReplyPath, body)
		w.Header().Set(auth.HeaderTimestamp, timestamp)
		w.Header().Set(auth.HeaderSignature, signature)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// storeSyncedSeries takes in the readings and the sealed buckets a peer sent.
//
// Neither needs a high-water mark of its own. Both are written by the identity
// of what they measure - a series, an instant or a bucket - so a row arriving
// twice is written over itself, and the sender's own mark is what keeps that
// from happening every round anyway.
func (s *Server) storeSyncedSeries(peerID int64, payload model.SyncPayload) error {
	if err := s.Store.StoreSamples(peerID, payload.Samples); err != nil {
		return err
	}
	return s.Store.StoreRollups(peerID, payload.Rollups)
}

// storeSyncedMetrics inserts only samples newer than what is already held for
// that peer. Peers resend an overlapping window every round, so this is what
// keeps the table free of duplicates.
func (s *Server) storeSyncedMetrics(peerID int64, payload model.SyncPayload) error {
	if len(payload.Metrics) == 0 {
		return nil
	}

	watermark, ok, err := s.Store.MaxSampleTimestamp(peerID, series.CPUPercent)
	if err != nil {
		return err
	}

	fresh := make([]model.Metric, 0, len(payload.Metrics))
	for _, m := range payload.Metrics {
		if ok && !m.Timestamp.After(watermark.Time) {
			continue
		}
		fresh = append(fresh, m)
	}

	_, err = s.Store.InsertMetrics(peerID, fresh)
	return err
}

// storeSyncedAvailability does the same for the reachability a peer reported,
// resolving the subject of each observation to a local row id. Those are the
// nodes the peer can see, which is not always the set this one knows, so an
// unfamiliar id is registered rather than dropped.
func (s *Server) storeSyncedAvailability(peerID int64, payload model.SyncPayload) error {
	if len(payload.Availability) == 0 {
		return nil
	}

	watermark, ok, err := s.Store.MaxSampleTimestamp(peerID, series.LatencyMs)
	if err != nil {
		return err
	}

	targets := map[string]int64{}
	fresh := make([]store.Observation, 0, len(payload.Availability))
	for _, a := range payload.Availability {
		if ok && !a.Timestamp.After(watermark.Time) {
			continue
		}
		to, resolved := targets[a.ToServerID]
		if !resolved {
			if to, err = s.Store.EnsureServer(a.ToServerID); err != nil {
				return err
			}
			targets[a.ToServerID] = to
		}
		fresh = append(fresh, store.Observation{
			ToServerID:  to,
			Timestamp:   a.Timestamp,
			IsAvailable: a.IsAvailable,
			LatencyMs:   a.LatencyMs,
			HTTPStatus:  a.HTTPStatus,
		})
	}

	return s.Store.InsertAvailability(peerID, fresh)
}

// storeSyncedNotices does the same for the alerts a peer reported having sent.
// This is what stops an outage from arriving in Telegram once per alerting node
// - each of them learns here what the others have already announced.
func (s *Server) storeSyncedNotices(peerID int64, payload model.SyncPayload) error {
	if len(payload.Notices) == 0 {
		return nil
	}

	watermark, ok, err := s.Store.MaxNoticeTimestamp(peerID)
	if err != nil {
		return err
	}

	targets := map[string]int64{}
	fresh := make([]store.Notice, 0, len(payload.Notices))
	for _, n := range payload.Notices {
		if ok && !n.Timestamp.After(watermark.Time) {
			continue
		}
		to, resolved := targets[n.ToServerID]
		if !resolved {
			if to, err = s.Store.EnsureServer(n.ToServerID); err != nil {
				return err
			}
			targets[n.ToServerID] = to
		}
		fresh = append(fresh, store.Notice{
			ToServerID: to,
			Timestamp:  n.Timestamp,
			IsDown:     n.IsDown,
		})
	}

	return s.Store.InsertNotices(peerID, fresh)
}

// ── Dashboard data ──────────────────────────────────────────────────────────

type serverView struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Location string        `json:"location"`
	URL      string        `json:"url"`
	IsSelf   bool          `json:"isSelf"`
	PushOnly bool          `json:"pushOnly"`
	LastSeen model.Time    `json:"lastSeen"`
	IsOnline bool          `json:"isOnline"`
	Latest   *model.Metric `json:"latest"`
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.Store.ListServers()
	if err != nil {
		s.fail(w, "list servers", err)
		return
	}

	views := make([]serverView, 0, len(servers))
	for _, srv := range servers {
		latest, err := s.Store.LatestMetricSeries(srv.ID)
		if err != nil {
			s.fail(w, "latest metric", err)
			return
		}

		online := srv.IsSelf
		if !srv.IsSelf {
			// Reachability is whatever this server last observed; a peer's own
			// opinion of itself is not evidence it is reachable from here.
			available, _, err := s.Store.LatestEdge(s.ServerID, srv.ID)
			if err != nil {
				s.fail(w, "latest availability", err)
				return
			}
			online = available
		}

		views = append(views, serverView{
			ID:       srv.UID,
			Name:     srv.Name,
			Location: srv.Location,
			URL:      srv.URL,
			IsSelf:   srv.IsSelf,
			PushOnly: srv.PushOnly,
			LastSeen: srv.LastSeen,
			IsOnline: online,
			Latest:   latest,
		})
	}

	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleServerMetrics(w http.ResponseWriter, r *http.Request) {
	srv, found, err := s.Store.ServerByUID(r.PathValue("id"))
	if err != nil {
		s.fail(w, "server metrics", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, []model.Metric{})
		return
	}

	window := hoursParam(r)
	since := model.At(time.Now().Add(-window))

	// Which rung to read, rather than how wide to group: the aggregate the
	// chart wants has already been computed, so the only question left is
	// which one. An hour or less is served from the raw checks, because the
	// live view is the one place the sampling cadence is the point.
	metrics, err := s.metricsFor(srv.ID, window, since)
	if err != nil {
		s.fail(w, "server metrics", err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) metricsFor(serverID int64, window time.Duration, since model.Time) ([]model.Metric, error) {
	level, aggregated := series.LevelFor(window)
	if !aggregated {
		return s.Store.MetricsRaw(serverID, since)
	}
	return s.Store.MetricsAtLevel(serverID, level,
		series.Bucket(since.DB(), level), series.Bucket(model.Now().DB(), level))
}

type matrixEntry struct {
	FromServerID        string     `json:"fromServerId"`
	ToServerID          string     `json:"toServerId"`
	AvailabilityPercent float64    `json:"availabilityPercent"`
	LastCheck           model.Time `json:"lastCheck"`
	LastLatencyMs       *float64   `json:"lastLatencyMs"`
	IsAvailable         bool       `json:"isAvailable"`
}

// matrixView is the matrix together with the span it covers.
//
// The window travels with the edges because the dashboard labels the view with
// it, and a copy of the number kept in the page is one that goes stale there
// without anybody noticing - which is how an empty cell came to be explained
// as "no checks in the last hour" long after the window had shrunk to two
// minutes.
type matrixView struct {
	WindowSeconds int           `json:"windowSeconds"`
	Edges         []matrixEntry `json:"edges"`
}

// handleAvailabilityMatrix draws every route in the mesh at once.
//
// It reads raw checks rather than the aggregates, and has to: a bucket is not
// written until it can no longer change, which is minutes after it ends, and a
// cell that went red a minute ago has to be red now.
func (s *Server) handleAvailabilityMatrix(w http.ResponseWriter, r *http.Request) {
	states, err := s.Store.EdgeStates(model.At(time.Now().Add(-matrixWindow)))
	if err != nil {
		s.fail(w, "availability matrix", err)
		return
	}

	entries := make([]matrixEntry, 0, len(states))
	for _, e := range states {
		entry := matrixEntry{
			FromServerID:  e.FromUID,
			ToServerID:    e.ToUID,
			LastCheck:     e.LastCheck,
			LastLatencyMs: e.LastLatency,
			IsAvailable:   e.LastWasUp,
		}
		if e.Checks > 0 {
			entry.AvailabilityPercent = round1(float64(e.Successes) / float64(e.Checks) * 100)
		}
		entries = append(entries, entry)
	}

	// Stable output keeps the rendered matrix from reshuffling between polls.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FromServerID != entries[j].FromServerID {
			return entries[i].FromServerID < entries[j].FromServerID
		}
		return entries[i].ToServerID < entries[j].ToServerID
	})

	writeJSON(w, http.StatusOK, matrixView{
		WindowSeconds: int(matrixWindow.Seconds()),
		Edges:         entries,
	})
}

// historyEntry is one point on a route's chart.
//
// Availability and MaxLatencyMs are new alongside the three the first build
// sent, and mean something only once a point covers an interval rather than an
// instant: how much of it the route was reachable for, and the worst any one
// check in it cost. A mean alone hides exactly the spike the chart is being
// looked at for.
type historyEntry struct {
	Timestamp    model.Time `json:"timestamp"`
	IsAvailable  bool       `json:"isAvailable"`
	LatencyMs    *float64   `json:"latencyMs"`
	MaxLatencyMs *float64   `json:"maxLatencyMs"`
	Availability float64    `json:"availability"`
}

func (s *Server) handleAvailabilityHistory(w http.ResponseWriter, r *http.Request) {
	from, fromKnown, err := s.Store.ServerByUID(r.PathValue("fromId"))
	if err != nil {
		s.fail(w, "availability history", err)
		return
	}
	to, toKnown, err := s.Store.ServerByUID(r.PathValue("toId"))
	if err != nil {
		s.fail(w, "availability history", err)
		return
	}
	if !fromKnown || !toKnown {
		writeJSON(w, http.StatusOK, []historyEntry{})
		return
	}

	window := hoursParam(r)
	since := model.At(time.Now().Add(-window))

	// The same choice of rung the metric charts make. This one used to be the
	// exception: it returned every stored check, so a week-long window meant
	// sixty thousand points for one route.
	var (
		points []store.EdgePoint
		err2   error
	)
	if level, aggregated := series.LevelFor(window); aggregated {
		points, err2 = s.Store.EdgeAtLevel(from.ID, to.ID, level,
			series.Bucket(since.DB(), level), series.Bucket(model.Now().DB(), level))
	} else {
		points, err2 = s.Store.EdgeRaw(from.ID, to.ID, since)
	}
	if err2 != nil {
		s.fail(w, "availability history", err2)
		return
	}

	entries := make([]historyEntry, 0, len(points))
	for _, p := range points {
		entries = append(entries, historyEntry{
			Timestamp:    p.Timestamp,
			IsAvailable:  p.IsAvailable,
			LatencyMs:    p.LatencyMs,
			MaxLatencyMs: p.MaxLatencyMs,
			Availability: p.Availability,
		})
	}
	writeJSON(w, http.StatusOK, entries)
}

// ── Peer management ─────────────────────────────────────────────────────────

type peerView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Location string     `json:"location"`
	URL      string     `json:"url"`
	PushOnly bool       `json:"pushOnly"`
	LastSeen model.Time `json:"lastSeen"`
}

func (s *Server) handleListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.Store.ListPeers()
	if err != nil {
		s.fail(w, "list peers", err)
		return
	}
	// Push-only peers have no address, which is what ListPeers selects on,
	// but they are peers all the same and removable like any other.
	pushing, err := s.Store.ListPushOnly()
	if err != nil {
		s.fail(w, "list push-only peers", err)
		return
	}

	views := make([]peerView, 0, len(peers)+len(pushing))
	for _, p := range append(peers, pushing...) {
		views = append(views, peerView{p.UID, p.Name, p.Location, p.URL, p.PushOnly, p.LastSeen})
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	url := peer.NormalizeURL(req.URL)
	if url == "" {
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	identity, err := s.Client.Introduce(ctx, url, model.IntroduceRequest{
		ServerID:   s.ServerUID,
		ServerName: s.ServerName,
		Location:   s.Location,
		SelfURL:    s.PublicURL,
		Alerts:     s.Alerts,
		PushOnly:   s.PushOnly,
	})
	if err != nil {
		s.Log.Warn("introduce to new peer failed", "url", url, "err", err)
		if errors.Is(err, peer.ErrUnauthorized) {
			http.Error(w, "The peer rejected our signature. Check that SharedSecret matches on both servers.", http.StatusBadRequest)
			return
		}
		http.Error(w, "Could not reach the peer. Make sure it is running and accessible.", http.StatusBadRequest)
		return
	}
	if identity.ServerID == s.ServerUID {
		http.Error(w, "That URL points back at this server.", http.StatusBadRequest)
		return
	}

	// Capture the mesh as it was before the new peer joined, so the
	// cross-introductions below do not include the newcomer twice.
	existing, err := s.Store.ListPeers()
	if err != nil {
		s.fail(w, "list peers", err)
		return
	}

	// Adding a node back is the one thing that lifts a removal, and it has to
	// happen before the peer is written: the tombstone is what keeps the row
	// out of every listing, so the address stored below would otherwise be
	// stored onto a node that stays invisible. The decision travels with this
	// node's next pushes, and the neighbours that still hold the removal stop
	// refusing the peer as it reaches them.
	if _, err := s.Store.SetMembership(identity.ServerID, false, model.Now()); err != nil {
		s.fail(w, "readmit peer", err)
		return
	}

	// LastSeen records when this server observed the peer, so it uses the local
	// clock rather than the timestamp the peer reported about itself.
	if _, err := s.Store.UpsertPeer(identity.ServerID, identity.ServerName, identity.Location, url,
		identity.Alerts, model.Now()); err != nil {
		s.fail(w, "store peer", err)
		return
	}

	s.crossIntroduce(ctx, existing, identity, url)

	writeJSON(w, http.StatusOK, peerView{
		ID:       identity.ServerID,
		Name:     identity.ServerName,
		Location: identity.Location,
		URL:      url,
	})
}

// crossIntroduce tells the established peers about the newcomer and the
// newcomer about them, so a mesh assembles from a single operator action.
func (s *Server) crossIntroduce(ctx context.Context, existing []model.Server, identity *model.HealthResponse, url string) {
	var wg sync.WaitGroup
	announce := func(target string, req model.IntroduceRequest) {
		defer wg.Done()
		if _, err := s.Client.Introduce(ctx, target, req); err != nil {
			s.Log.Debug("cross-introduction failed", "target", target, "err", err)
		}
	}

	for _, p := range existing {
		if p.UID == identity.ServerID || p.URL == "" {
			continue
		}
		wg.Add(2)
		go announce(p.URL, model.IntroduceRequest{
			ServerID:   identity.ServerID,
			ServerName: identity.ServerName,
			Location:   identity.Location,
			SelfURL:    url,
			Alerts:     identity.Alerts,
		})
		go announce(url, model.IntroduceRequest{
			ServerID:   p.UID,
			ServerName: p.Name,
			Location:   p.Location,
			SelfURL:    p.URL,
			Alerts:     p.Alerts,
		})
	}
	wg.Wait()
}

// handleRemovePeer takes a node out of the mesh - here and, as the decision
// travels in this node's pushes, everywhere else.
//
// Nothing is deleted. The row stays as the record of the removal: it is what
// stops the node being learned back from a neighbour that has not heard yet,
// what stops the node itself registering again with its next push, and what
// there is to tell the other nodes with. Its history stays too, unreadable from
// the dashboard because the node is no longer listed, and ages out with
// retention like anything else.
func (s *Server) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	srv, found, err := s.Store.ServerByUID(r.PathValue("id"))
	if err != nil {
		s.fail(w, "get server", err)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if srv.IsSelf {
		http.Error(w, "Cannot remove self", http.StatusBadRequest)
		return
	}

	if _, err := s.Store.SetMembership(srv.UID, true, model.Now()); err != nil {
		s.fail(w, "remove peer", err)
		return
	}
	s.Log.Info("removed peer from mesh", "peer", srv.Name, "uid", srv.UID)
	w.WriteHeader(http.StatusOK)
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// hoursParam reads the ?hours= window, clamped the same way the dashboard's
// presets expect: at least one hour, at most a week, defaulting to a day.
func hoursParam(r *http.Request) time.Duration {
	hours, err := strconv.Atoi(r.URL.Query().Get("hours"))
	if err != nil || hours == 0 {
		hours = 24
	}
	return time.Duration(max(1, min(hours, 168))) * time.Hour
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer io.Copy(io.Discard, body)

	if err := json.NewDecoder(body).Decode(dst); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
