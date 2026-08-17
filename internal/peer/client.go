// Package peer talks to other m0nit0r instances.
package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/m0n5ter/m0nit0r/internal/auth"
	"github.com/m0n5ter/m0nit0r/internal/model"
)

// API paths on the remote peer.
const (
	pathIntroduce = "/api/introduce"
	pathSync      = "/api/sync"
)

// ErrUnauthorized reports that the peer rejected our signature, which almost
// always means the two ends have different shared secrets.
var ErrUnauthorized = errors.New("peer rejected our signature")

// Client issues the two outbound calls this app makes: introducing itself to a
// peer, and pushing accumulated data to one.
type Client struct {
	http   *http.Client
	signer *auth.Signer
}

// New returns a Client that signs its requests with the given shared secret.
// An empty secret sends unsigned requests. The timeout is short enough that one
// unreachable peer cannot stall a sync round.
func New(secret string) *Client {
	return &Client{
		http:   &http.Client{Timeout: 15 * time.Second},
		signer: auth.New(secret),
	}
}

// NormalizeURL trims a trailing slash so stored addresses compare equal.
func NormalizeURL(url string) string {
	return strings.TrimRight(strings.TrimSpace(url), "/")
}

// Introduce announces this server to the peer at baseURL and returns the peer's
// identity.
func (c *Client) Introduce(ctx context.Context, baseURL string, req model.IntroduceRequest) (*model.HealthResponse, error) {
	var health model.HealthResponse
	if err := c.postJSON(ctx, NormalizeURL(baseURL), pathIntroduce, req, &health); err != nil {
		return nil, err
	}
	if health.ServerID == "" {
		return nil, fmt.Errorf("peer at %s returned no server id", baseURL)
	}
	return &health, nil
}

// PushSync sends payload to a peer. The measured round trip is also this
// server's availability probe for that peer, so a failure is a normal result
// rather than an error: latency and status are reported either way.
func (c *Client) PushSync(ctx context.Context, baseURL string, payload model.SyncPayload) (ok bool, latencyMs float64, status *int) {
	start := time.Now()

	body, err := json.Marshal(payload)
	if err != nil {
		return false, 0, nil
	}

	req, err := c.newRequest(ctx, NormalizeURL(baseURL), pathSync, body)
	if err != nil {
		return false, elapsedMs(start), nil
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return false, elapsedMs(start), nil
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	code := resp.StatusCode
	return code >= 200 && code < 300, elapsedMs(start), &code
}

// newRequest builds a signed POST for the given API path.
func (c *Client) newRequest(ctx context.Context, baseURL, apiPath string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+apiPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if c.signer.Enabled() {
		timestamp, signature := c.signer.Sign(apiPath, body)
		req.Header.Set(auth.HeaderTimestamp, timestamp)
		req.Header.Set(auth.HeaderSignature, signature)
	}
	return req, nil
}

func (c *Client) postJSON(ctx context.Context, baseURL, apiPath string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := c.newRequest(ctx, baseURL, apiPath, body)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", baseURL+apiPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("post %s: %s", baseURL+apiPath, resp.Status)
	}
	if out == nil {
		return nil
	}

	// Cap the response so a hostile or broken peer cannot exhaust memory here.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", baseURL+apiPath, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", baseURL+apiPath, err)
	}
	return nil
}

func elapsedMs(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}
