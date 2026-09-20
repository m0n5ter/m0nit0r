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

// ReplyPath is what the reply to a sync is signed over in place of a request
// path. It is not one any request can carry, so a signature lifted from a
// request cannot be passed off as a reply, nor a reply's as a request.
const ReplyPath = pathSync + "#reply"

// maxReplyBytes caps a sync reply, which names peers and nothing else.
const maxReplyBytes = 1 << 20

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

// PushResult is the outcome of one push. The measured round trip is also this
// server's availability probe for that peer, so a failure is a normal result
// rather than an error: latency and status are reported either way.
//
// Reply is the peer list the peer answered with, when its answer carried a
// valid signature; it is nil otherwise, which is also what a peer too old to
// answer with one yields.
type PushResult struct {
	OK        bool
	LatencyMs float64
	Status    *int
	Reply     *model.SyncReply
}

// PushSync sends payload to a peer.
func (c *Client) PushSync(ctx context.Context, baseURL string, payload model.SyncPayload) PushResult {
	start := time.Now()

	body, err := json.Marshal(payload)
	if err != nil {
		return PushResult{}
	}

	req, err := c.newRequest(ctx, NormalizeURL(baseURL), pathSync, body)
	if err != nil {
		return PushResult{LatencyMs: elapsedMs(start)}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return PushResult{LatencyMs: elapsedMs(start)}
	}
	defer resp.Body.Close()

	// Read before the clock stops, so the latency covers the whole exchange
	// the way it did when the body was only drained.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))

	code := resp.StatusCode
	result := PushResult{OK: code >= 200 && code < 300, LatencyMs: elapsedMs(start), Status: &code}
	if result.OK && len(raw) > 0 {
		result.Reply = c.readReply(resp.Header, raw)
	}
	return result
}

// readReply accepts a sync reply only when it is signed with the shared
// secret. The peers it names are where this node will send its own data next,
// so an unsigned list would let anybody on the path redirect it.
func (c *Client) readReply(header http.Header, raw []byte) *model.SyncReply {
	if err := c.signer.Verify(header.Get(auth.HeaderTimestamp), header.Get(auth.HeaderSignature),
		ReplyPath, raw, time.Now()); err != nil {
		return nil
	}
	var reply model.SyncReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil
	}
	return &reply
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
