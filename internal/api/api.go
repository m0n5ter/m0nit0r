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
type Server struct {
	Store      *store.Store
	Client     *peer.Client
	Signer     *auth.Signer
	ServerID   string
	ServerName string
	Location   string
	PublicURL  string
	Log        *slog.Logger
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
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

	return mux
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
		ServerID:   s.ServerID,
		ServerName: s.ServerName,
		Location:   s.Location,
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
	// storing, so record the caller only when both id and URL are present.
	if req.ServerID != "" && req.ServerID != s.ServerID && peer.NormalizeURL(req.SelfURL) != "" {
		if err := s.Store.UpsertPeer(req.ServerID, req.ServerName, req.Location,
			peer.NormalizeURL(req.SelfURL), model.Now()); err != nil {
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
	if payload.ServerID == s.ServerID {
		http.Error(w, "refusing to sync a payload from this server's own id", http.StatusBadRequest)
		return
	}

	if err := s.Store.UpsertPeer(payload.ServerID, payload.ServerName, payload.Location,
		peer.NormalizeURL(payload.SelfURL), model.Now()); err != nil {
		s.Log.Error("record syncing peer", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to record peer", http.StatusInternalServerError)
		return
	}

	if err := s.storeSyncedMetrics(payload); err != nil {
		s.Log.Error("store synced metrics", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to store metrics", http.StatusInternalServerError)
		return
	}
	if err := s.storeSyncedAvailability(payload); err != nil {
		s.Log.Error("store synced availability", "peer", payload.ServerID, "err", err)
		http.Error(w, "failed to store availability", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// storeSyncedMetrics inserts only samples newer than what is already held for
// that peer. Peers resend an overlapping window every round, so this is what
// keeps the table free of duplicates.
func (s *Server) storeSyncedMetrics(payload model.SyncPayload) error {
	if len(payload.Metrics) == 0 {
		return nil
	}

	watermark, ok, err := s.Store.MaxMetricTimestamp(payload.ServerID)
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

	_, err = s.Store.InsertMetrics(payload.ServerID, fresh)
	return err
}

func (s *Server) storeSyncedAvailability(payload model.SyncPayload) error {
	if len(payload.Availability) == 0 {
		return nil
	}

	watermark, ok, err := s.Store.MaxAvailabilityTimestamp(payload.ServerID)
	if err != nil {
		return err
	}

	fresh := make([]model.Availability, 0, len(payload.Availability))
	for _, a := range payload.Availability {
		if ok && !a.Timestamp.After(watermark.Time) {
			continue
		}
		fresh = append(fresh, a)
	}

	return s.Store.InsertAvailability(payload.ServerID, fresh)
}

// ── Dashboard data ──────────────────────────────────────────────────────────

type serverView struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Location string        `json:"location"`
	URL      string        `json:"url"`
	IsSelf   bool          `json:"isSelf"`
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
			last, err := s.Store.LatestAvailability(s.ServerID, srv.ID)
			if err != nil {
				s.fail(w, "latest availability", err)
				return
			}
			online = last != nil && last.IsAvailable
		}

		views = append(views, serverView{
			ID:       srv.ID,
			Name:     srv.Name,
			Location: srv.Location,
			URL:      srv.URL,
			IsSelf:   srv.IsSelf,
			LastSeen: srv.LastSeen,
			IsOnline: online,
			Latest:   latest,
		})
	}

	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleServerMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since := model.At(time.Now().Add(-hoursParam(r)))

	metrics, err := s.Store.MetricsSince(id, since)
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
		key := edge{row.FromServerID, row.ToServerID}
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
	since := model.At(time.Now().Add(-hoursParam(r)))

	rows, err := s.Store.AvailabilityHistory(r.PathValue("fromId"), r.PathValue("toId"), since)
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
	LastSeen model.Time `json:"lastSeen"`
}

func (s *Server) handleListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.Store.ListPeers()
	if err != nil {
		s.fail(w, "list peers", err)
		return
	}

	views := make([]peerView, 0, len(peers))
	for _, p := range peers {
		views = append(views, peerView{p.ID, p.Name, p.Location, p.URL, p.LastSeen})
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
		ServerID:   s.ServerID,
		ServerName: s.ServerName,
		Location:   s.Location,
		SelfURL:    s.PublicURL,
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
	if identity.ServerID == s.ServerID {
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
	if err := s.Store.UpsertPeer(identity.ServerID, identity.ServerName, identity.Location, url, model.Now()); err != nil {
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
		if p.ID == identity.ServerID || p.URL == "" {
			continue
		}
		wg.Add(2)
		go announce(p.URL, model.IntroduceRequest{
			ServerID:   identity.ServerID,
			ServerName: identity.ServerName,
			Location:   identity.Location,
			SelfURL:    url,
		})
		go announce(url, model.IntroduceRequest{
			ServerID:   p.ID,
			ServerName: p.Name,
			Location:   p.Location,
			SelfURL:    p.URL,
		})
	}
	wg.Wait()
}

func (s *Server) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	srv, found, err := s.Store.GetServer(id)
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
	// is what the dashboard warns the operator will happen.
	if err := s.Store.SetPeerURL(id, ""); err != nil {
		s.fail(w, "clear peer url", err)
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
