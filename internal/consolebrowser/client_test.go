package consolebrowser

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientFixedEndpointsAndSecretIsolation(t *testing.T) {
	var calls atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer broker-only" || r.Header.Get("Cookie") != "" {
			t.Error("wrong internal credentials")
		}
		if r.URL.Path != "/sessions" || r.Method != "POST" {
			t.Errorf("unexpected endpoint %s", r.URL)
		}
		var input map[string]string
		_ = json.NewDecoder(r.Body).Decode(&input)
		if len(input) != 1 || input["provider"] != "claude" {
			t.Error("unexpected body")
		}
		_ = json.NewEncoder(w).Encode(Session{ID: "opaque-id", Provider: "claude", Status: "ready", ExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer broker.Close()
	client, err := NewClient(broker.URL, "broker-only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Create(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../secret", "https://evil.test", "x?secret=1", "x/y", ""} {
		if _, err = client.Get(context.Background(), id); err == nil {
			t.Errorf("accepted unsafe ID %q", id)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unsafe ID reached broker")
	}
}
func TestClientRejectsRedirectAndSanitizesErrors(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer other.Close()
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/secret", 302) }))
	defer broker.Close()
	client, _ := NewClient(broker.URL, "broker-only")
	_, err := client.Create(context.Background(), "claude")
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), broker.URL) || leaked.Load() {
		t.Fatalf("unsafe redirect handling: %v", err)
	}
}
func TestClientDisabledAndURLValidation(t *testing.T) {
	for _, raw := range []string{"https://user:pass@example.com", "http://host/path", "http://host?x=1", "ftp://host", "http://host#fragment"} {
		if _, err := NewClient(raw, "secret"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	t.Setenv("CLIPROXY_CONSOLE_BROWSER_URL", "")
	t.Setenv("CLIPROXY_CONSOLE_BROWSER_TOKEN_FILE", "")
	if NewClientFromEnv() != nil {
		t.Fatal("missing configuration should disable browser")
	}
}
func TestClientViewerRequiresBinarySubprotocol(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer broker.Close()
	client, _ := NewClient(broker.URL, "secret")
	conn, err := client.View(context.Background(), "opaque")
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("accepted broker without binary protocol")
	}
}
