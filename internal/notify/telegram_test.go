package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// local returns a sender pointed at ts instead of the real bot API.
func local(ts *httptest.Server) *Telegram {
	t := NewTelegram("123:secret", "-1001234567890")
	t.base = ts.URL
	t.http = ts.Client()
	return t
}

func TestSendPostsTheMessageToTheBotAPI(t *testing.T) {
	var (
		path string
		body map[string]any
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	if err := local(ts).Send(t.Context(), "🔴 <b>BG</b> is unreachable"); err != nil {
		t.Fatal(err)
	}

	if want := "/bot123:secret/sendMessage"; path != want {
		t.Errorf("posted to %q, want %q", path, want)
	}
	if body["chat_id"] != "-1001234567890" {
		t.Errorf("chat_id is %v, want the configured chat", body["chat_id"])
	}
	if body["parse_mode"] != "HTML" {
		t.Errorf("parse_mode is %v, want HTML - the messages carry <b> tags", body["parse_mode"])
	}
	if body["text"] != "🔴 <b>BG</b> is unreachable" {
		t.Errorf("text is %v, want it sent through unchanged", body["text"])
	}
}

// A rejected message has to say why. The reason is only in the body - the
// status alone is the same 400 for a bad token and for unparseable markup - and
// the bot token must not be carried out with it.
func TestSendReportsWhyTheAPIRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"ok":false,"description":"chat not found"}`, http.StatusBadRequest)
	}))
	defer ts.Close()

	err := local(ts).Send(t.Context(), "anything")
	if err == nil {
		t.Fatal("Send accepted a 400 as delivery")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error is %q, want it to carry the API's reason", err)
	}
	if strings.Contains(err.Error(), "123:secret") {
		t.Errorf("error is %q, and puts the bot token in the log", err)
	}
}

func TestSendReportsATransportFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := ts.Client()
	ts.Close()

	tg := NewTelegram("123:secret", "chat")
	tg.base, tg.http = ts.URL, client
	if err := tg.Send(context.Background(), "anything"); err == nil {
		t.Fatal("Send reported success against a closed listener")
	}
}

// A server named with an ampersand or an angle bracket would otherwise make
// Telegram reject the whole message as unparseable HTML - so the alert about
// that node would be the one alert that never arrives.
func TestEscapeCoversWhatTelegramTreatsAsMarkup(t *testing.T) {
	got := Escape(`R&D <lab>`)
	if want := "R&amp;D &lt;lab&gt;"; got != want {
		t.Errorf("Escape produced %q, want %q", got, want)
	}
}
