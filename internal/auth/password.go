package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Lockout policy for the dashboard password. A handful of typos is free; after
// that each further failure from the same address doubles how long it has to
// wait, up to a ceiling that keeps a guessing run at a few attempts an hour.
const (
	freeAttempts = 5
	firstLockout = time.Minute
	maxLockout   = 15 * time.Minute
	// forgetAfter is how long an address has to stay quiet before its failures
	// are forgotten and it starts again from the free attempts.
	forgetAfter = time.Hour
	// maxTracked bounds the table of addresses, so a spray of guesses from
	// many sources cannot grow it without limit.
	maxTracked = 10000
)

// SessionCookie names the cookie the dashboard's login form earns.
const SessionCookie = "m0nit0r_session"

// sessionLifetime is how long one login lasts in a browser.
const sessionLifetime = 30 * 24 * time.Hour

// Gate checks the password people present: the dashboard through its own login
// form, which trades it for a session cookie, and the Android app and scripts
// as HTTP basic authentication, whose user name is not checked - there is one
// password for the whole mesh and nothing to tell users apart by.
//
// It is a separate credential from the shared secret on purpose. Over plain
// HTTP a password crosses the wire as it is, where the shared secret never
// does; a password that leaked this way must not also let its holder sign the
// peer protocol.
type Gate struct {
	sum [sha256.Size]byte
	on  bool
	// sessionKey signs session cookies. It is derived from the password, so
	// changing the password signs everybody out, and it needs no storage, so
	// sessions survive a restart.
	sessionKey []byte

	mu      sync.Mutex
	strikes map[string]*strike
	now     func() time.Time
}

type strike struct {
	failures int
	last     time.Time
	until    time.Time
}

// NewGate returns a Gate for the given password. An empty password disables it
// and lets everything through.
func NewGate(password string) *Gate {
	g := &Gate{strikes: map[string]*strike{}, now: time.Now}
	if password != "" {
		g.sum = sha256.Sum256([]byte(password))
		g.on = true
		key := sha256.Sum256([]byte("m0nit0r-session\n" + password))
		g.sessionKey = key[:]
	}
	return g
}

// Enabled reports whether a password was configured.
func (g *Gate) Enabled() bool { return g.on }

// Result is the outcome of one check.
type Result int

const (
	Allowed Result = iota
	Denied
	// LockedOut means the address has failed too often and is not being
	// listened to at all for now, correct password or not - otherwise the
	// lockout would only slow a guessing run down rather than stop it.
	LockedOut
)

// Check decides one request. Requests from the host itself are let through
// without a password: whoever can open a loopback connection can already read
// appsettings.json, and it is how the deployment script manages peers and how a
// push-only node's own dashboard is reached.
//
// A valid session cookie is enough; otherwise basic authentication is tried,
// and a request carrying neither is denied without counting as a guess - it is
// the dashboard finding out that it has to ask.
func (g *Gate) Check(r *http.Request) (Result, time.Duration) {
	if !g.on || IsLoopback(r.RemoteAddr) {
		return Allowed, 0
	}
	if c, err := r.Cookie(SessionCookie); err == nil && g.validSession(c.Value) {
		return Allowed, 0
	}
	_, password, supplied := r.BasicAuth()
	if !supplied {
		return Denied, 0
	}
	return g.Try(r.RemoteAddr, password)
}

// Try checks one password offered from remoteAddr, counting it towards that
// address's lockout when it is wrong. It is what the login form calls, and
// what basic authentication goes through.
func (g *Gate) Try(remoteAddr, password string) (Result, time.Duration) {
	if !g.on {
		return Allowed, 0
	}

	host := clientHost(remoteAddr)
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.strikes[host]
	if s != nil && now.Before(s.until) {
		return LockedOut, s.until.Sub(now)
	}

	sum := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(sum[:], g.sum[:]) == 1 {
		delete(g.strikes, host)
		return Allowed, 0
	}

	if s == nil || now.Sub(s.last) > forgetAfter {
		g.prune(now)
		s = &strike{}
		g.strikes[host] = s
	}
	s.failures++
	s.last = now
	if over := s.failures - freeAttempts; over > 0 {
		wait := firstLockout << min(over-1, 10)
		s.until = now.Add(min(wait, maxLockout))
	}
	return Denied, 0
}

// NewSession returns the cookie a successful login sets. The value is its own
// expiry and a signature over it, so the server keeps no session table.
//
// remember is the "remember me" box: with it the cookie is stored until it
// expires, without it the browser is told nothing about its lifetime, which
// makes it a session cookie the browser drops when it closes. The signature
// carries the same expiry either way - the difference is what the browser
// keeps, not what the server will accept.
//
// HttpOnly keeps it out of reach of scripts; SameSite=Strict keeps other sites
// from making the browser send it, which is what stops a page elsewhere from
// adding or removing peers through a signed-in browser. It cannot be marked
// Secure while the agent speaks plain HTTP.
func (g *Gate) NewSession(remember bool) *http.Cookie {
	expires := g.now().Add(sessionLifetime)
	value := strconv.FormatInt(expires.Unix(), 10)
	cookie := &http.Cookie{
		Name:     SessionCookie,
		Value:    value + "." + g.signSession(value),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	if remember {
		cookie.Expires = expires
		cookie.MaxAge = int(sessionLifetime.Seconds())
	}
	return cookie
}

// EndSession returns the cookie that signs a browser out.
func EndSession() *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

func (g *Gate) signSession(expiry string) string {
	mac := hmac.New(sha256.New, g.sessionKey)
	mac.Write([]byte("session\n" + expiry))
	return hex.EncodeToString(mac.Sum(nil))
}

func (g *Gate) validSession(value string) bool {
	expiry, signature, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || !g.now().Before(time.Unix(unix, 0)) {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	want, _ := hex.DecodeString(g.signSession(expiry))
	return hmac.Equal(got, want)
}

// prune drops forgotten addresses once the table is full, and failing that
// the whole table: losing the count on some guessers is better than holding
// memory for all of them.
func (g *Gate) prune(now time.Time) {
	if len(g.strikes) < maxTracked {
		return
	}
	for host, s := range g.strikes {
		if now.Sub(s.last) > forgetAfter && now.After(s.until) {
			delete(g.strikes, host)
		}
	}
	if len(g.strikes) >= maxTracked {
		g.strikes = map[string]*strike{}
	}
}

func clientHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// IsLoopback reports whether a request came from the host itself.
func IsLoopback(remoteAddr string) bool {
	ip := net.ParseIP(clientHost(remoteAddr))
	return ip != nil && ip.IsLoopback()
}
