package worker

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
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
			available, found, err := s.LatestAvailability(w.ServerID, roamer)
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
	if _, found, err := s.LatestAvailability(w.ServerID, roamer); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("an observation was recorded before the grace period since start-up had passed")
	}
}

// TestLearnPeersAddsOnlyTheUnknown: the push-only node learns the mesh from the
// replies to its pushes, but a peer its operator removed has to stay removed
// however often a neighbour mentions it.
func TestLearnPeersAddsOnlyTheUnknown(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.UpsertSelf(testRoamUID, "roamer", "Car", "", false); err != nil {
		t.Fatal(err)
	}
	removed, err := s.UpsertPeer(testOtherUID, "removed", "Lab", "http://removed:5001", false, model.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPeerURL(removed, ""); err != nil {
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
