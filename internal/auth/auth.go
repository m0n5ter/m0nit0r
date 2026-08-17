// Package auth signs and verifies the peer protocol with a shared secret.
//
// Only the endpoints that write to the database are covered: /api/sync and
// /api/introduce. The dashboard's read endpoints are served to a browser, which
// has no way to hold the secret, so protecting them would need a session login
// rather than a message signature.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Header names carrying the signature and the time it was produced.
const (
	HeaderTimestamp = "X-M0nit0r-Timestamp"
	HeaderSignature = "X-M0nit0r-Signature"
)

// maxSkew bounds how far a request's timestamp may be from the receiver's
// clock. It has to absorb ordinary NTP drift between hosts while keeping the
// window in which a captured request stays usable short.
//
// Within that window a captured request can still be replayed. That is
// tolerable here because both signed endpoints are idempotent: repeated
// metrics are dropped by the receiver's timestamp watermark, and a repeated
// introduction rewrites a peer row with the same values.
const maxSkew = 5 * time.Minute

// ErrNoSignature reports a request that carried no signature at all.
var ErrNoSignature = errors.New("request is not signed")

// Signer produces and checks request signatures. A Signer built from an empty
// secret is disabled and verifies nothing.
type Signer struct {
	secret []byte
}

// New returns a Signer for the given shared secret. An empty secret disables
// authentication.
func New(secret string) *Signer {
	if secret == "" {
		return &Signer{}
	}
	return &Signer{secret: []byte(secret)}
}

// Enabled reports whether a secret was configured.
func (s *Signer) Enabled() bool { return len(s.secret) > 0 }

// Sign returns the timestamp and signature headers for a request body.
func (s *Signer) Sign(apiPath string, body []byte) (timestamp, signature string) {
	timestamp = strconv.FormatInt(time.Now().Unix(), 10)
	return timestamp, s.compute(timestamp, apiPath, body)
}

// Verify checks a request's signature and freshness. It returns nil when the
// signature is valid, and an error describing the problem otherwise.
func (s *Signer) Verify(timestamp, signature, apiPath string, body []byte, now time.Time) error {
	if !s.Enabled() {
		return nil
	}
	if timestamp == "" || signature == "" {
		return ErrNoSignature
	}

	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("malformed timestamp: %w", err)
	}

	skew := now.Sub(time.Unix(unix, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > maxSkew {
		return fmt.Errorf("timestamp is %s off; check clock synchronisation", skew.Round(time.Second))
	}

	expected := s.compute(timestamp, apiPath, body)
	// Compare the decoded bytes so that a mismatch cannot be narrowed down by
	// timing the comparison.
	got, err := hex.DecodeString(signature)
	if err != nil {
		return errors.New("malformed signature")
	}
	want, _ := hex.DecodeString(expected)

	if !hmac.Equal(got, want) {
		return errors.New("signature mismatch")
	}
	return nil
}

// compute derives the signature over the timestamp, the request path and the
// exact body bytes. Binding the path prevents a signature captured from one
// endpoint being replayed against another.
func (s *Signer) compute(timestamp, apiPath string, body []byte) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("\n"))
	mac.Write([]byte(apiPath))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
