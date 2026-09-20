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
	"github.com/m0n5ter/m0nit0r/internal/store"
	"github.com/m0n5ter/m0nit0r/internal/web"
)

// maxBodyBytes caps inbound request bodies. Sync payloads from a busy peer are
// the largest legitimate input and stay far below this.
const maxBodyBytes = 8 << 20

// matrixWindow is how far back the availability matrix looks.
const matrixWindow = time.Hour

// matrixSamples is how many recent checks per edge feed the percentage.
const matrixSamples = 10

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

	peerID, err := s.Store.UpsertPeer(payload.ServerID, payload.ServerName, payload.Location,
		peer.NormalizeURL(payload.SelfURL), payload.Alerts, model.Now())
	if err == nil {
		err = s.Store.SetPushOnly(peerID, payload.PushOnly)
	}
	if err != nil {
		s.Log.Error("record syncing peer", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to record peer", http.StatusInternalServerError)
		return
	}

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
	if err := s.storeSyncedNotices(peerID, payload); err != nil {
		s.Log.Error("store synced notices", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to store notices", http.StatusInternalServerError)
		return
	}

	s.replyWithPeers(w, payload.ServerID)
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

// storeSyncedMetrics inserts only samples newer than what is already held for
// that peer. Peers resend an overlapping window every round, so this is what
// keeps the table free of duplicates.
func (s *Server) storeSyncedMetrics(peerID int64, payload model.SyncPayload) error {
	if len(payload.Metrics) == 0 {
		return nil
	}

	watermark, ok, err := s.Store.MaxMetricTimestamp(peerID)
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

	watermark, ok, err := s.Store.MaxAvailabilityTimestamp(peerID)
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
		latest, err := s.Store.LatestMetric(srv.ID)
		if err != nil {
			s.fail(w, "latest metric", err)
			return
		}

		online := srv.IsSelf
		if !srv.IsSelf {
			// Reachability is whatever this server last observed; a peer's own
			// opinion of itself is not evidence it is reachable from here.
			available, _, err := s.Store.LatestAvailability(s.ServerID, srv.ID)
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

	metrics, err := s.Store.MetricsBucketed(srv.ID, since, bucketFor(window))
	if err != nil {
		s.fail(w, "server metrics", err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

type matrixEntry struct {
	FromServerID        string     `json:"fromServerId"`
	ToServerID          string     `json:"toServerId"`
	AvailabilityPercent float64    `json:"availabilityPercent"`
	LastCheck           model.Time `json:"lastCheck"`
	LastLatencyMs       *float64   `json:"lastLatencyMs"`
	IsAvailable         bool       `json:"isAvailable"`
}

func (s *Server) handleAvailabilityMatrix(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.AvailabilitySince(model.At(time.Now().Add(-matrixWindow)))
	if err != nil {
		s.fail(w, "availability matrix", err)
		return
	}

	type edge struct{ from, to string }
	grouped := map[edge][]store.AvailabilityRow{}
	for _, row := range rows {
		key := edge{row.FromUID, row.ToUID}
		grouped[key] = append(grouped[key], row)
	}

	entries := make([]matrixEntry, 0, len(grouped))
	for key, group := range grouped {
		sort.Slice(group, func(i, j int) bool {
			return group[i].Timestamp.After(group[j].Timestamp.Time)
		})
		if len(group) > matrixSamples {
			group = group[:matrixSamples]
		}

		up := 0
		for _, row := range group {
			if row.IsAvailable {
				up++
			}
		}

		last := group[0]
		entries = append(entries, matrixEntry{
			FromServerID:        key.from,
			ToServerID:          key.to,
			AvailabilityPercent: round1(float64(up) / float64(len(group)) * 100),
			LastCheck:           last.Timestamp,
			LastLatencyMs:       last.LatencyMs,
			IsAvailable:         last.IsAvailable,
		})
	}

	// Stable output keeps the rendered matrix from reshuffling between polls.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FromServerID != entries[j].FromServerID {
			return entries[i].FromServerID < entries[j].FromServerID
		}
		return entries[i].ToServerID < entries[j].ToServerID
	})

	writeJSON(w, http.StatusOK, entries)
}

type historyEntry struct {
	Timestamp   model.Time `json:"timestamp"`
	IsAvailable bool       `json:"isAvailable"`
	LatencyMs   *float64   `json:"latencyMs"`
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

	since := model.At(time.Now().Add(-hoursParam(r)))

	rows, err := s.Store.AvailabilityHistory(from.ID, to.ID, since)
	if err != nil {
		s.fail(w, "availability history", err)
		return
	}

	entries := make([]historyEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, historyEntry{row.Timestamp, row.IsAvailable, row.LatencyMs})
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

	// Clearing the address stops syncing but keeps the collected history, which
	// is what the dashboard warns the operator will happen. A push-only peer
	// has no address to clear; dropping the flag is what stops it being
	// watched for pushes. Either comes back if the node keeps syncing here.
	if err := s.Store.SetPeerURL(srv.ID, ""); err != nil {
		s.fail(w, "clear peer url", err)
		return
	}
	if err := s.Store.SetPushOnly(srv.ID, false); err != nil {
		s.fail(w, "clear push-only", err)
		return
	}
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

// maxPoints is roughly how many samples a chart in the dashboard should have
// to draw. Chosen for the eye rather than the renderer: past a few hundred
// points a line is denser than the plot is wide, so the extra samples cost
// time without showing anything.
const maxPoints = 360

// bucketFor picks how wide a metric bucket has to be for a window of this
// length to stay that size. An hour is served raw, because the live view is
// the one place the five-second cadence is the point; every longer window is
// rounded up to a bucket a person can name, so the tooltip reads as "one of
// the fifteen-minute averages" rather than an arbitrary span.
func bucketFor(window time.Duration) time.Duration {
	if window <= time.Hour {
		return 0
	}
	for _, bucket := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute} {
		if window/maxPoints <= bucket {
			return bucket
		}
	}
	return time.Hour
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
