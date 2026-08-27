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
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	n := &node{uid: uid, store: st}

	api := &Server{
		Store:      st,
		Client:     peer.New(""),
		Signer:     auth.New(""),
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
	ok, _, status := peer.New("").PushSync(t.Context(), to.server.URL, payload)
	if !ok {
		t.Fatalf("sync to %s failed, status %v", to.uid, status)
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
