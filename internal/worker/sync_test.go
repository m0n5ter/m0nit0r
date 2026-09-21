package worker

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/peer"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

const (
	testSelfUID  = "11111111-1111-4111-8111-111111111111"
	testRoamUID  = "22222222-2222-4222-8222-222222222222"
	testOtherUID = "33333333-3333-4333-8333-333333333333"
)

// hubWithRoamer is a store holding this node and one push-only peer that last
// pushed here at lastPush.
func hubWithRoamer(t *testing.T, lastPush time.Time) (*store.Store, *Sync, int64) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	self, err := s.UpsertSelf(testSelfUID, "hub", "Lab", "http://hub:5001", false)
	if err != nil {
		t.Fatal(err)
	}
	roamer, err := s.UpsertPeer(testRoamUID, "roamer", "Car", "", false, model.At(lastPush))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPushOnly(roamer, true); err != nil {
		t.Fatal(err)
	}

	w := &Sync{
		Store:     s,
		ServerID:  self,
		ServerUID: testSelfUID,
		Interval:  10 * time.Second,
		Log:       slog.New(slog.DiscardHandler),
	}
	return s, w, roamer
}

// TestWatchPushOnlyJudgesAPeerByItsPushes: a push-only peer has no address to
// probe, so the round's record of it is whether it has been heard from lately.
// That record is what the matrix, the online dot and the alerts all read.
func TestWatchPushOnlyJudgesAPeerByItsPushes(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name     string
		lastPush time.Duration
		want     bool
	}{
		{"pushed a round ago", 10 * time.Second, true},
		{"missed two rounds", 25 * time.Second, true},
		{"silent past the grace", 2 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w, roamer := hubWithRoamer(t, now.Add(-tc.lastPush))
			w.started = now.Add(-time.Hour)

			if err := w.watchPushOnly(model.At(now)); err != nil {
				t.Fatal(err)
			}
			available, found, err := s.LatestEdge(w.ServerID, roamer)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("no observation was recorded")
			}
			if available != tc.want {
				t.Errorf("recorded available=%v, want %v", available, tc.want)
			}
		})
	}
}

// TestWatchPushOnlyWaitsOutItsOwnRestart: straight after this node starts,
// every push-only peer looks as silent as this node was down. That is not an
// outage of the peer and must not be recorded as one.
func TestWatchPushOnlyWaitsOutItsOwnRestart(t *testing.T) {
	now := time.Now()
	s, w, roamer := hubWithRoamer(t, now.Add(-time.Hour))
	w.started = now.Add(-5 * time.Second)

	if err := w.watchPushOnly(model.At(now)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LatestEdge(w.ServerID, roamer); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("an observation was recorded before the grace period since start-up had passed")
	}
}

// TestLearnPeersAddsOnlyTheUnknown: a node learns the mesh from the replies to
// its pushes, but a peer its operator removed has to stay removed however often
// a neighbour mentions it.
func TestLearnPeersAddsOnlyTheUnknown(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.UpsertSelf(testRoamUID, "roamer", "Car", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPeer(testOtherUID, "removed", "Lab", "http://removed:5001", false, model.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembership(testOtherUID, true, model.Now()); err != nil {
		t.Fatal(err)
	}

	w := &Sync{Store: s, ServerUID: testRoamUID, PushOnly: true, Log: slog.New(slog.DiscardHandler)}
	w.learnPeers([]*model.SyncReply{nil, {Peers: []model.PeerInfo{
		{ServerID: testSelfUID, ServerName: "hub", URL: "http://hub:5001/"},
		{ServerID: testOtherUID, ServerName: "removed", URL: "http://removed:5001"},
		{ServerID: testRoamUID, ServerName: "roamer", URL: "http://mirror:5001"},
	}}})

	peers, err := s.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].UID != testSelfUID {
		t.Fatalf("peers after learning = %+v, want only the hub", peers)
	}
	if peers[0].URL != "http://hub:5001" {
		t.Errorf("learned address %q was not normalised", peers[0].URL)
	}
}

// TestLearnPeersCompletesAPlaceholder: the ids of nodes this one has not met
// arrive first inside a neighbour's reachability reports, which leaves a row
// named after the id and reachable at nothing. Being told who that is has to
// finish the row, or the mesh stays a pair of nodes surrounded by bare ids.
func TestLearnPeersCompletesAPlaceholder(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.UpsertSelf(testSelfUID, "hub", "Lab", "http://hub:5001", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureServer(testOtherUID); err != nil {
		t.Fatal(err)
	}

	w := &Sync{Store: s, ServerUID: testSelfUID, Log: slog.New(slog.DiscardHandler)}
	w.learnPeers([]*model.SyncReply{{Peers: []model.PeerInfo{
		{ServerID: testOtherUID, ServerName: "third", Location: "Roof", URL: "http://third:5001"},
	}}})

	peers, err := s.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("peers after learning = %+v, want the completed placeholder", peers)
	}
	if peers[0].Name != "third" || peers[0].Location != "Roof" || peers[0].URL != "http://third:5001" {
		t.Errorf("the placeholder was not completed: %+v", peers[0])
	}
}

// TestPushToAPeerThatRemovedUsDropsIt: a removed node is not pushed to and not
// probed, so the refusal of its own push is the only way it is ever told. It
// has to act on it - and not file it as an outage, since nothing is down.
func TestPushDroppedByAPeerThatRemovedThisNode(t *testing.T) {
	s, w, _ := hubWithRoamer(t, time.Now())
	w.Client = peer.New("")

	gone := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "this server was removed from the mesh", http.StatusGone)
	}))
	t.Cleanup(gone.Close)

	target, err := s.UpsertPeer(testOtherUID, "former", "Lab", gone.URL, false, model.Now())
	if err != nil {
		t.Fatal(err)
	}

	w.push(t.Context(), model.Server{ID: target, UID: testOtherUID, Name: "former", URL: gone.URL},
		model.SyncPayload{ServerID: testSelfUID}, model.Now())

	peers, err := s.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		if p.UID == testOtherUID {
			t.Errorf("still syncing with the peer that turned this node away: %+v", p)
		}
	}
	history, err := s.EdgeRaw(w.ServerID, target, model.At(time.Now().Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Errorf("being removed was recorded as %d outages: %+v", len(history), history)
	}
	// The decision was somebody else's, so nothing is announced from here -
	// and the peer's own first push back in is enough to restore them.
	if decisions, err := s.Membership(); err != nil || len(decisions) != 0 {
		t.Errorf("dropping the peer published %+v, err=%v, want nothing", decisions, err)
	}
}

// TestRoundProbesReachEveryPeer: a round's probes are stamped with the instant
// the round began but written only once the pushes come back, after the
// round's payload has already been cut. A reading taken in between - the metric
// ticker fires on the same five-second beat as the round, so one lands a few
// milliseconds after it began on every round or none - must not carry the
// peer's high-water mark past the probes still to be written, or they are
// never sent and the node's whole row goes blank on every other dashboard.
func TestRoundProbesReachEveryPeer(t *testing.T) {
	s, w, _ := hubWithRoamer(t, time.Now())
	w.peerMarks = map[int64]seriesMarks{}

	target, err := s.UpsertPeer(testOtherUID, "other", "Lab", "http://other:5001", false, model.Now())
	if err != nil {
		t.Fatal(err)
	}
	peerRow := model.Server{ID: target, UID: testOtherUID, Protocol: model.ProtocolVersion}

	round := time.Now().Truncate(time.Second)

	// The metric ticker, a millisecond into the round.
	if _, err := s.InsertMetrics(w.ServerID, []model.Metric{{
		Timestamp: model.At(round.Add(time.Millisecond)), CpuPercent: 5,
	}}); err != nil {
		t.Fatal(err)
	}

	_, sent, err := w.forPeer(model.SyncPayload{}, peerRow, model.At(round))
	if err != nil {
		t.Fatal(err)
	}
	w.peerMarks[target] = sent

	// The push comes back and the round's probe is recorded, stamped with the
	// round's own instant.
	latency := 42.0
	if err := s.InsertAvailability(w.ServerID, []store.Observation{{
		ToServerID: target, Timestamp: model.At(round), IsAvailable: true, LatencyMs: &latency,
	}}); err != nil {
		t.Fatal(err)
	}

	next, _, err := w.forPeer(model.SyncPayload{}, peerRow, model.At(round.Add(w.Interval)))
	if err != nil {
		t.Fatal(err)
	}
	for _, smp := range next.Samples {
		if smp.Peer == testOtherUID && smp.Timestamp.Equal(round) {
			return
		}
	}
	t.Fatalf("the probe taken at the start of the round was never offered; next round carried %d samples", len(next.Samples))
}
