// Package notify delivers alerts to the outside world. Telegram is the only
// destination today; nothing above this package knows that.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// apiBase is where the bot API lives. A field rather than a constant only so
// the tests can point it at a local server.
const apiBase = "https://api.telegram.org"

// Telegram posts messages to one chat as one bot.
type Telegram struct {
	token  string
	chatID string
	base   string
	http   *http.Client
}

// NewTelegram returns a sender for the given bot and chat.
//
// The timeout is generous compared with the peer client's: an alert is sent
// once every few minutes at worst, from a loop that has nothing else to do
// while it waits, and giving up early on a slow API call would mean an outage
// went unreported.
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		token:  token,
		chatID: chatID,
		base:   apiBase,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Send delivers one message, formatted as Telegram's restricted HTML.
func (t *Telegram) Send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id": t.chatID,
		"text":    text,
		// The messages carry hostnames and IP addresses; without this Telegram
		// tries to unfurl them and pads every alert with a link preview card.
		"link_preview_options": map[string]any{"is_disabled": true},
		"parse_mode":           "HTML",
	})
	if err != nil {
		return fmt.Errorf("encode telegram message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.base+"/bot"+t.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("send telegram message: %w", err)
	}
	defer resp.Body.Close()

	// Read either way: on failure the body is the only place the reason - a
	// revoked token, a chat the bot was removed from - is stated, and the token
	// itself must not be echoed with it, which is why the URL is not in the
	// error.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram API: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// Escape renders text safe to place inside a message. Telegram's HTML mode
// treats these three characters as markup and rejects the whole message if it
// cannot parse them, so a server named with an ampersand would otherwise stop
// its own alerts from being delivered.
func Escape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}
