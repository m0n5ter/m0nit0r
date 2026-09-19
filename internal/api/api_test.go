package api

import (
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/auth"
	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/peer"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// The dashboard's presets are the windows this has to answer for, and what
// matters is the same for each: a bucket a person can name, and few enough
// points that the chart is drawing data rather than pixels. The hour is
// exempt by design - it is the live view, where every sample is the point.
func TestBucketForKeepsThePresetsDrawable(t *testing.T) {
	for _, tc := range []struct {
		hours int
		want  time.Duration
	}{
		{1, 0},
		{6, time.Minute},
		{24, 5 * time.Minute},
		{72, 15 * time.Minute},
		{168, 30 * time.Minute},
	} {
		window := time.Duration(tc.hours) * time.Hour

		got := bucketFor(window)
		if got != tc.want {
			t.Errorf("bucketFor(%dh) = %v, want %v", tc.hours, got, tc.want)
			continue
		}
		if got == 0 {
			continue
		}
		if points := int(window / got); points > maxPoints {
			t.Errorf("%dh in %v buckets is %d points, over the %d budget", tc.hours, got, points, maxPoints)
		}
	}
}

// node is one m0nit0r instance served over a real HTTP listener, which is what
// makes the exchange below the actual peer protocol rather than two stores
// sharing a process.
type node struct {
	uid    string
	id     int64
	store  *store.Store
	server *httptest.Server
}

func newNode(t *testing.T, uid, name string, alerts bool) *node {
	return newSecretNode(t, uid, name, alerts, "")
}

// newSecretNode is newNode for a mesh that signs its protocol with secret.
func newSecretNode(t *testing.T, uid, name string, alerts bool, secret string) *node {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	n := &node{uid: uid, store: st}

	api := &Server{
		Store:      st,
		Client:     peer.New(secret),
		Signer:     auth.New(secret),
		ServerUID:  uid,
		ServerName: name,
		Location:   "Lab",
		Alerts:     alerts,
		Log:        slog.New(slog.DiscardHandler),
	}
	n.server = httptest.NewServer(api.Handler())
	t.Cleanup(n.server.Close)

	if n.id, err = st.UpsertSelf(uid, name, "Lab", n.server.URL, alerts); err != nil {
		t.Fatal(err)
	}
	api.ServerID = n.id
	api.PublicURL = n.server.URL
	return n
}

// sync pushes one payload from n to the peer, the way the sync worker does.
func (n *node) sync(t *testing.T, to *node, payload model.SyncPayload) {
	t.Helper()

	payload.ServerID = n.uid
	payload.SelfURL = n.server.URL
	if res := peer.New("").PushSync(t.Context(), to.server.URL, payload); !res.OK {
		t.Fatalf("sync to %s failed, status %v", to.uid, res.Status)
	}
}

// TestAlertNoticesReachTheOtherAlertingNodes is what keeps one outage from
// arriving in Telegram once per alerting node. The suppression is only as good
// as this hop: a node stays quiet because it can see somebody else already
// spoke, and it only ever sees that if the notice crossed the wire.
func TestAlertNoticesReachTheOtherAlertingNodes(t *testing.T) {
	first := newNode(t, "11111111-1111-4111-8111-111111111111", "first", true)
	second := newNode(t, "22222222-2222-4222-8222-222222222222", "second", true)
	victim := "33333333-3333-4333-8333-333333333333"

	// The first node reports the third one down, and the second node has not
	// heard of the third one at all - which is the ordinary case for a mesh
	// still assembling, and the reason a notice may not name a known server.
	announced := model.Now()
	first.sync(t, second, model.SyncPayload{
		Alerts:  true,
		Notices: []model.Notice{{ToServerID: victim, Timestamp: announced, IsDown: true}},
	})

	// The second node now has to be able to answer "has anybody reported this?"
	// about a node it learned of from the notice itself.
	target, err := second.store.EnsureServer(victim)
	if err != nil {
		t.Fatal(err)
	}
	last, found, err := second.store.LatestNotice(target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the second node sees no notice, and would send the same message again")
	}
	if !last.IsDown {
		t.Error("the notice arrived as a recovery, not as the outage that was reported")
	}

	// And it has to know the first node is one of the alerting ones, or it
	// would take the first place in the stagger for itself and act at the same
	// moment rather than behind it.
	servers, err := second.store.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	alerting := 0
	for _, srv := range servers {
		if srv.Alerts {
			alerting++
		}
	}
	if alerting != 2 {
		t.Errorf("%d nodes count as alerting, want both of them: %+v", alerting, servers)
	}

	// Replayed the way every sync round replays its overlapping window. A
	// second copy would make the outage look freshly reported an hour from now
	// and delay the repeat that is meant to fire then.
	first.sync(t, second, model.SyncPayload{
		Alerts:  true,
		Notices: []model.Notice{{ToServerID: victim, Timestamp: announced, IsDown: true}},
	})

	origin, _, err := second.store.ServerByUID(first.uid)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := second.store.OwnNoticesSince(origin.ID, model.At(announced.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Errorf("the replayed round left %d notices, want the one: %+v", len(stored), stored)
	}
}

// TestPushOnlyNodeIsRecordedAndToldAboutTheMesh covers the one hop a push-only
// node depends on. It cannot be reached, so it is never introduced to anybody:
// everything it learns about the mesh arrives in the replies to its own pushes,
// and everything the mesh learns about it arrives in those pushes.
func TestPushOnlyNodeIsRecordedAndToldAboutTheMesh(t *testing.T) {
	const secret = "s3cret"
	hub := newSecretNode(t, "11111111-1111-4111-8111-111111111111", "hub", false, secret)
	other := newSecretNode(t, "22222222-2222-4222-8222-222222222222", "other", false, secret)
	roamer := "33333333-3333-4333-8333-333333333333"

	if _, err := hub.store.UpsertPeer(other.uid, "other", "Lab", other.server.URL, false, model.Now()); err != nil {
		t.Fatal(err)
	}
	// The hub once knew the roamer at an address, before it lost it. Being
	// told the node is push-only now has to stop the hub pushing there.
	if _, err := hub.store.UpsertPeer(roamer, "roamer", "Car", "http://10.0.0.9:5001", false, model.Now()); err != nil {
		t.Fatal(err)
	}

	res := peer.New(secret).PushSync(t.Context(), hub.server.URL, model.SyncPayload{
		ServerID:   roamer,
		ServerName: "roamer",
		Location:   "Car",
		PushOnly:   true,
		Metrics:    []model.Metric{{Timestamp: model.Now(), CpuPercent: 12}},
	})
	if !res.OK {
		t.Fatalf("push failed, status %v", res.Status)
	}

	if res.Reply == nil {
		t.Fatal("the push-only node got no signed reply, so it can never learn of the rest of the mesh")
	}
	var named []string
	for _, p := range res.Reply.Peers {
		named = append(named, p.ServerID)
		if p.ServerID == other.uid && p.URL != other.server.URL {
			t.Errorf("other named at %q, want %q", p.URL, other.server.URL)
		}
	}
	if len(named) != 1 || named[0] != other.uid {
		t.Errorf("reply names %v, want only the other node", named)
	}

	srv, found, err := hub.store.ServerByUID(roamer)
	if err != nil || !found {
		t.Fatalf("hub has no row for the roamer: found=%v err=%v", found, err)
	}
	if !srv.PushOnly {
		t.Error("the hub does not know the roamer is push-only, and would never watch for its pushes")
	}
	if srv.URL != "" {
		t.Errorf("the roamer kept the address %q, and the hub would go on pushing to it", srv.URL)
	}

	peers, err := hub.store.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		if p.UID == roamer {
			t.Error("the roamer is still among the peers the hub pushes to")
		}
	}

	latest, err := hub.store.LatestMetric(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.CpuPercent != 12 {
		t.Errorf("the roamer's metrics did not arrive: %+v", latest)
	}
}

// TestUnsignedSyncReplyIsIgnored: the peers a reply names are where the
// push-only node will send its data next, so a reply the shared secret does not
// vouch for must not be acted on - even when the push itself went through.
func TestUnsignedSyncReplyIsIgnored(t *testing.T) {
	// A hub without the secret signs nothing, and accepts anything.
	hub := newNode(t, "11111111-1111-4111-8111-111111111111", "hub", false)
	if _, err := hub.store.UpsertPeer("22222222-2222-4222-8222-222222222222", "other", "Lab",
		"http://elsewhere:5001", false, model.Now()); err != nil {
		t.Fatal(err)
	}

	res := peer.New("s3cret").PushSync(t.Context(), hub.server.URL, model.SyncPayload{
		ServerID: "33333333-3333-4333-8333-333333333333",
		PushOnly: true,
	})
	if !res.OK {
		t.Fatalf("push failed, status %v", res.Status)
	}
	if res.Reply != nil {
		t.Errorf("an unsigned reply was accepted: %+v", res.Reply)
	}
}
