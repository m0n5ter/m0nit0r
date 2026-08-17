package auth

import (
	"errors"
	"testing"
	"time"
)

const (
	secret  = "correct-horse-battery-staple"
	apiPath = "/api/sync"
)

var body = []byte(`{"serverId":"abc","metrics":[]}`)

func TestVerifyAcceptsOwnSignature(t *testing.T) {
	s := New(secret)

	timestamp, signature := s.Sign(apiPath, body)
	if err := s.Verify(timestamp, signature, apiPath, body, time.Now()); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	signer := New(secret)
	timestamp, signature := signer.Sign(apiPath, body)

	tests := []struct {
		name      string
		verifier  *Signer
		path      string
		body      []byte
		signature string
	}{
		{"different secret", New("some-other-secret"), apiPath, body, signature},
		{"modified body", signer, apiPath, []byte(`{"serverId":"abc","metrics":[1]}`), signature},
		{"replayed at another endpoint", signer, "/api/introduce", body, signature},
		{"corrupted signature", signer, apiPath, body, "00" + signature[2:]},
		{"non-hex signature", signer, apiPath, body, "not-hex"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.verifier.Verify(timestamp, tc.signature, tc.path, tc.body, time.Now()); err == nil {
				t.Fatal("expected verification to fail, got nil")
			}
		})
	}
}

func TestVerifyRejectsStaleTimestamp(t *testing.T) {
	s := New(secret)
	timestamp, signature := s.Sign(apiPath, body)

	// Both directions matter: a receiver whose clock runs behind must not
	// accept a signature minted far in its future either.
	for _, skew := range []time.Duration{maxSkew + time.Minute, -(maxSkew + time.Minute)} {
		if err := s.Verify(timestamp, signature, apiPath, body, time.Now().Add(skew)); err == nil {
			t.Fatalf("expected failure at skew %s, got nil", skew)
		}
	}

	// Drift smaller than the window is normal between hosts and must pass.
	if err := s.Verify(timestamp, signature, apiPath, body, time.Now().Add(maxSkew-time.Minute)); err != nil {
		t.Fatalf("rejected acceptable clock drift: %v", err)
	}
}

func TestVerifyRejectsMissingHeaders(t *testing.T) {
	s := New(secret)
	timestamp, signature := s.Sign(apiPath, body)

	for _, tc := range []struct{ timestamp, signature string }{
		{"", ""},
		{timestamp, ""},
		{"", signature},
	} {
		err := s.Verify(tc.timestamp, tc.signature, apiPath, body, time.Now())
		if !errors.Is(err, ErrNoSignature) {
			t.Fatalf("timestamp=%q signature=%q: got %v, want ErrNoSignature", tc.timestamp, tc.signature, err)
		}
	}
}

func TestDisabledSignerAcceptsAnything(t *testing.T) {
	s := New("")

	if s.Enabled() {
		t.Fatal("signer built from an empty secret reports itself enabled")
	}
	if err := s.Verify("", "", apiPath, body, time.Now()); err != nil {
		t.Fatalf("disabled signer rejected an unsigned request: %v", err)
	}
}

func TestMalformedTimestampIsRejected(t *testing.T) {
	s := New(secret)
	_, signature := s.Sign(apiPath, body)

	if err := s.Verify("not-a-number", signature, apiPath, body, time.Now()); err == nil {
		t.Fatal("expected malformed timestamp to fail, got nil")
	}
}
