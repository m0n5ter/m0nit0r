package worker

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"github.com/m0n5ter/m0nit0r/internal/store"
)

// The thresholds every case below is written against.
const (
	testDownAfter = 2 * time.Minute
	testRepeat    = time.Hour
	testStagger   = time.Minute
)

// TestDecide walks the alerting rule through the situations it exists for. The
// pairs of cases that differ only in delay are the ones that matter most: they
// are what keeps two alerting nodes from both sending the same message, and
// what lets the second one send it when the first has gone quiet.
func TestDecide(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	// ago builds a timestamp d before the fixed "now".
	ago := func(d time.Duration) model.Time { return model.At(now.Add(-d)) }

	// downNotice and upNotice are what somebody in the mesh last announced.
	downNotice := func(d time.Duration) store.Notice {
		return store.Notice{Timestamp: ago(d), IsDown: true}
	}
	upNotice := func(d time.Duration) store.Notice {
		return store.Notice{Timestamp: ago(d), IsDown: false}
	}

	cases := []struct {
		name      string
		available bool
		since     model.Time
		last      store.Notice
		announced bool
		delay     time.Duration
		wantSend  bool
		wantDown  bool
	}{{
		name:      "a peer that has always answered is not news",
		available: true,
		since:     ago(3 * time.Hour),
	}, {
		name:      "a blip shorter than the threshold is not announced",
		available: false,
		since:     ago(90 * time.Second),
	}, {
		name:      "an outage past the threshold nobody has reported is announced",
		available: false,
		since:     ago(3 * time.Minute),
		wantSend:  true,
		wantDown:  true,
	}, {
		// The stagger in full: at two and a half minutes the first-ranked node
		// has already spoken and the second-ranked one has not reached its own
		// threshold yet, which is the gap the first one's notice arrives in.
		name:      "the second-ranked node holds off past the plain threshold",
		available: false,
		since:     ago(150 * time.Second),
		delay:     testStagger,
	}, {
		name:      "the second-ranked node speaks when nobody else has",
		available: false,
		since:     ago(190 * time.Second),
		delay:     testStagger,
		wantSend:  true,
		wantDown:  true,
	}, {
		name:      "an outage somebody has already reported is left alone",
		available: false,
		since:     ago(20 * time.Minute),
		last:      downNotice(18 * time.Minute),
		announced: true,
	}, {
		name:      "a report older than the repeat window is repeated",
		available: false,
		since:     ago(3 * time.Hour),
		last:      downNotice(61 * time.Minute),
		announced: true,
		wantSend:  true,
		wantDown:  true,
	}, {
		// Without the stagger on the repeat window too, every alerting node
		// would come due for the hourly repeat in the same second, and the
		// notice that suppresses the duplicate would arrive too late to.
		name:      "the second-ranked node holds off on the repeat as well",
		available: false,
		since:     ago(3 * time.Hour),
		// Past the plain repeat window, so the case above would send here, and
		// short of that window plus one stagger, which is what this one waits
		// for. The gap between the two is where the first node's notice lands.
		last:      downNotice(60*time.Minute + 30*time.Second),
		announced: true,
		delay:     testStagger,
	}, {
		name:      "the second-ranked node repeats when nobody else has",
		available: false,
		since:     ago(3 * time.Hour),
		last:      downNotice(70 * time.Minute),
		announced: true,
		delay:     testStagger,
		wantSend:  true,
		wantDown:  true,
	}, {
		name:      "recovery from a reported outage is announced",
		available: true,
		since:     ago(20 * time.Second),
		last:      downNotice(15 * time.Minute),
		announced: true,
		wantSend:  true,
	}, {
		name:      "recovery already announced is not announced again",
		available: true,
		since:     ago(20 * time.Second),
		last:      upNotice(10 * time.Second),
		announced: true,
	}, {
		// A peer this node could always reach, that somebody else reported
		// down. The outage is on the other node's route, so this one has no
		// recovery to announce - its run of successful checks predates the
		// report rather than following it.
		name:      "a recovery this node never saw a failure for is not announced",
		available: true,
		since:     ago(6 * time.Hour),
		last:      downNotice(4 * time.Minute),
		announced: true,
	}, {
		name:      "the second-ranked node holds off on the recovery",
		available: true,
		since:     ago(20 * time.Second),
		last:      downNotice(15 * time.Minute),
		announced: true,
		delay:     testStagger,
	}, {
		name:      "the second-ranked node announces the recovery when nobody else has",
		available: true,
		since:     ago(90 * time.Second),
		last:      downNotice(15 * time.Minute),
		announced: true,
		delay:     testStagger,
		wantSend:  true,
	}}

	w := &Alerts{DownAfter: testDownAfter, Repeat: testRepeat, Stagger: testStagger}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			down, send := w.decide(tc.available, tc.since, tc.last, tc.announced, now, tc.delay)
			if send != tc.wantSend {
				t.Fatalf("send is %v, want %v", send, tc.wantSend)
			}
			if send && down != tc.wantDown {
				t.Errorf("down is %v, want %v", down, tc.wantDown)
			}
		})
	}
}

// TestRankDelay checks that the order is taken over the alerting nodes alone
// and agreed on by all of them: every node computes it from the same replicated
// register, so a node that ranked itself by a different rule would collide with
// the others rather than take a place behind them.
func TestRankDelay(t *testing.T) {
	// Deliberately not in UID order, and with a non-alerting node interleaved,
	// which is the ordinary case: most of a mesh has no Telegram credentials.
	servers := []model.Server{
		{UID: "ccc", Alerts: true},
		{UID: "aaa", Alerts: true},
		{UID: "bbb", Alerts: false},
	}

	for uid, want := range map[string]time.Duration{
		"aaa": 0,
		"ccc": testStagger,
		// Not an alerting node, and so not in the order at all. Nothing calls
		// this for such a node, but answering "first" rather than panicking is
		// what makes a register read mid-write cost a duplicate message
		// instead of a crashed worker.
		"bbb": 0,
	} {
		w := &Alerts{ServerUID: uid, Stagger: testStagger}
		if got := w.rankDelay(servers); got != want {
			t.Errorf("%s ranks at %v, want %v", uid, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{2*time.Minute + 5*time.Second, "2m 5s"},
		{74 * time.Minute, "1h 14m"},
		{50 * time.Hour, "2d 2h"},
	} {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) is %q, want %q", tc.in, got, tc.want)
		}
	}
}

// recorder stands in for Telegram and keeps what it was handed.
type recorder struct {
	sent []string
	err  error
}

func (r *recorder) Send(_ context.Context, text string) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, text)
	return nil
}

// alertingNode is a store holding this node and one peer, with a route between
// them that has been failing for the given time.
func alertingNode(t *testing.T, downFor time.Duration) (*store.Store, int64, model.Server) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	self, err := s.UpsertSelf("11111111-1111-4111-8111-111111111111", "here", "Lab", "http://here:5001", true)
	if err != nil {
		t.Fatal(err)
	}
	peerID, err := s.UpsertPeer("22222222-2222-4222-8222-222222222222", "BG", "45.39.253.23",
		"http://there:5001", false, model.Now())
	if err != nil {
		t.Fatal(err)
	}

	// One success just before the run of failures, so the outage has a
	// beginning to be measured from rather than reaching back to the first
	// record ever written.
	now := time.Now()
	records := []store.Observation{
		{ToServerID: peerID, Timestamp: model.At(now.Add(-downFor - 10*time.Second)), IsAvailable: true},
	}
	for at := downFor; at > 0; at -= 10 * time.Second {
		records = append(records, store.Observation{
			ToServerID: peerID,
			Timestamp:  model.At(now.Add(-at)),
		})
	}
	if err := s.InsertAvailability(self, records); err != nil {
		t.Fatal(err)
	}

	return s, self, model.Server{ID: peerID, UID: "22222222-2222-4222-8222-222222222222",
		Name: "BG", Location: "45.39.253.23", URL: "http://there:5001"}
}

func newAlerts(s *store.Store, self int64, to Sender) *Alerts {
	return &Alerts{
		Store:      s,
		Telegram:   to,
		ServerID:   self,
		ServerUID:  "11111111-1111-4111-8111-111111111111",
		ServerName: "here",
		DownAfter:  testDownAfter,
		Repeat:     testRepeat,
		Stagger:    testStagger,
		Log:        slog.New(slog.DiscardHandler),
	}
}

// TestConsiderAnnouncesAnOutageOnceAgainstRealStorage runs the whole path the
// ticker runs - reading the route out of SQLite, deciding, sending, recording -
// because the parts that unit tests hold apart are exactly where a duplicate
// would come from: the notice written by the send is what silences the next
// pass, and only storage connects the two.
func TestConsiderAnnouncesAnOutageOnceAgainstRealStorage(t *testing.T) {
	s, self, peer := alertingNode(t, 5*time.Minute)
	sent := &recorder{}
	w := newAlerts(s, self, sent)

	if err := w.consider(t.Context(), peer, 0); err != nil {
		t.Fatal(err)
	}
	if len(sent.sent) != 1 {
		t.Fatalf("the first pass sent %d messages, want the one", len(sent.sent))
	}

	message := sent.sent[0]
	for _, want := range []string{"BG", "unreachable", "45.39.253.23", "Seen from: here"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q:\n%s", want, message)
		}
	}

	// Everything about the peer is the same a tick later; only the notice the
	// first pass wrote is different, and it has to be enough.
	if err := w.consider(t.Context(), peer, 0); err != nil {
		t.Fatal(err)
	}
	if len(sent.sent) != 1 {
		t.Errorf("the outage was announced %d times, want once until the repeat falls due", len(sent.sent))
	}
}

// An undelivered message must not be recorded as sent: the record is what the
// whole mesh reads as "this has been reported", and one written for a message
// nobody received would silence the retry here and the other alerting nodes
// with it.
func TestConsiderRecordsNothingWhenDeliveryFails(t *testing.T) {
	s, self, peer := alertingNode(t, 5*time.Minute)
	failing := &recorder{err: errors.New("telegram API: 502 Bad Gateway")}
	w := newAlerts(s, self, failing)

	if err := w.consider(t.Context(), peer, 0); err == nil {
		t.Fatal("consider reported success after the send failed")
	}

	if _, announced, err := s.LatestNotice(peer.ID); err != nil {
		t.Fatal(err)
	} else if announced {
		t.Error("a notice was recorded for a message that was never delivered")
	}

	// Which is to say the next tick tries again.
	sent := &recorder{}
	w.Telegram = sent
	if err := w.consider(t.Context(), peer, 0); err != nil {
		t.Fatal(err)
	}
	if len(sent.sent) != 1 {
		t.Errorf("the retry sent %d messages, want the one", len(sent.sent))
	}
}

func TestConsiderIsSilentAboutAPeerItNeverProbed(t *testing.T) {
	s, self, peer := alertingNode(t, 0)
	sent := &recorder{}

	// A node this one has only been told about: registered, never measured.
	unknown, err := s.EnsureServer("33333333-3333-4333-8333-333333333333")
	if err != nil {
		t.Fatal(err)
	}
	peer.ID = unknown

	if err := newAlerts(s, self, sent).consider(t.Context(), peer, 0); err != nil {
		t.Fatal(err)
	}
	if len(sent.sent) != 0 {
		t.Errorf("sent %v about a peer that was never probed", sent.sent)
	}
}
