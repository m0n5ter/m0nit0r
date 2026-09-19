package auth

import (
	"net/http/httptest"
	"testing"
	"time"
)

func try(g *Gate, remote, password string) Result {
	r := httptest.NewRequest("GET", "/api/servers", nil)
	r.RemoteAddr = remote
	if password != "" {
		r.SetBasicAuth("admin", password)
	}
	res, _ := g.Check(r)
	return res
}

func TestGateChecksThePasswordAndIgnoresTheUser(t *testing.T) {
	g := NewGate("hunter2")

	if got := try(g, "203.0.113.5:4000", "hunter2"); got != Allowed {
		t.Errorf("correct password: %v, want Allowed", got)
	}
	if got := try(g, "203.0.113.5:4000", "hunter3"); got != Denied {
		t.Errorf("wrong password: %v, want Denied", got)
	}
	if got := try(g, "203.0.113.5:4000", ""); got != Denied {
		t.Errorf("no credentials: %v, want Denied", got)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:4000"
	r.SetBasicAuth("anybody", "hunter2")
	if got, _ := g.Check(r); got != Allowed {
		t.Errorf("another user name: %v, want Allowed", got)
	}
}

func TestGateLetsTheHostItselfThrough(t *testing.T) {
	g := NewGate("hunter2")
	for _, remote := range []string{"127.0.0.1:5555", "[::1]:5555"} {
		if got := try(g, remote, ""); got != Allowed {
			t.Errorf("%s without a password: %v, want Allowed", remote, got)
		}
	}
}

func TestDisabledGateLetsEverythingThrough(t *testing.T) {
	if got := try(NewGate(""), "203.0.113.5:4000", ""); got != Allowed {
		t.Errorf("got %v, want Allowed", got)
	}
}

// TestGateLocksOutAGuesser: past the free attempts an address is refused
// outright, the right password included - otherwise the lockout would only slow
// a guessing run down - and it is let back in once the wait has passed.
func TestGateLocksOutAGuesser(t *testing.T) {
	g := NewGate("hunter2")
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }

	const guesser = "198.51.100.7:1234"
	for i := 0; i < freeAttempts; i++ {
		if got := try(g, guesser, "wrong"); got != Denied {
			t.Fatalf("attempt %d: %v, want Denied", i+1, got)
		}
	}
	if got := try(g, guesser, "hunter2"); got != Allowed {
		t.Fatalf("right password within the free attempts: %v, want Allowed", got)
	}

	// The success reset the count; spend it again and one more.
	for i := 0; i <= freeAttempts; i++ {
		try(g, guesser, "wrong")
	}
	if got := try(g, guesser, "hunter2"); got != LockedOut {
		t.Fatalf("right password during the lockout: %v, want LockedOut", got)
	}
	if got := try(g, "198.51.100.8:1234", "hunter2"); got != Allowed {
		t.Errorf("another address is locked out too: %v", got)
	}

	now = now.Add(firstLockout + time.Second)
	if got := try(g, guesser, "hunter2"); got != Allowed {
		t.Errorf("after the lockout: %v, want Allowed", got)
	}
}

// TestGateLockoutGrowsAndIsCapped: each failure past the free ones doubles the
// wait, and no wait exceeds the ceiling.
func TestGateLockoutGrowsAndIsCapped(t *testing.T) {
	g := NewGate("hunter2")
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }

	const guesser = "198.51.100.7:1234"
	var waits []time.Duration
	for i := 0; i < freeAttempts+8; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = guesser
		r.SetBasicAuth("", "wrong")
		g.Check(r)
		if s := g.strikes["198.51.100.7"]; s != nil && s.until.After(now) {
			waits = append(waits, s.until.Sub(now))
			now = s.until // wait it out, then guess again
		}
	}

	if len(waits) == 0 || waits[0] != firstLockout {
		t.Fatalf("waits %v, want the first to be %v", waits, firstLockout)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] < waits[i-1] || waits[i] > maxLockout {
			t.Errorf("waits %v do not grow up to the %v ceiling", waits, maxLockout)
			break
		}
	}
	if waits[len(waits)-1] != maxLockout {
		t.Errorf("last wait %v, want the %v ceiling", waits[len(waits)-1], maxLockout)
	}
}
