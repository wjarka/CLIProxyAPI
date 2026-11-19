package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginCardsAndRedemption(t *testing.T) {
	now := time.Unix(1795080000, 0)
	expiry := now.Add(time.Hour)
	var pollToken string
	var redemptions int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(data any) {
			t.Helper()
			if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
				t.Error(err)
			}
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/oauth/cli/init":
			pollToken = r.Header.Get("Authorization")
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["provider"] != "zai" ||
				!strings.HasPrefix(pollToken, "Bearer ") || len(strings.TrimPrefix(pollToken, "Bearer ")) != 64 {
				t.Error("invalid login initialization")
			}
			send(map[string]any{"flow_id": "upstream-flow", "authorize_url": "https://chat.z.ai/login",
				"expires_at": now.Add(time.Minute).Unix(), "poll_interval_sec": 2})
		case "GET /api/v1/oauth/cli/poll/upstream-flow":
			if r.Header.Get("Authorization") != pollToken {
				t.Error("poll secret was not reused")
			}
			send(map[string]any{"status": "ready", "token": "zcode-private", "zai": map[string]string{"access_token": "zai-private"},
				"user": map[string]string{"user_id": "account", "name": "Plan owner"}})
		case "GET /api/v1/coding-plan/reset/status", "POST /api/v1/coding-plan/reset/use":
			if r.Header.Get("Authorization") != "Bearer zcode-private" ||
				r.Header.Get("X-Bigmodel-Authorization") != "zai-private" || r.Header.Get("Bigmodel-Target-Type") != "PERSONAL" {
				t.Error("expected account-scoped OAuth credentials")
			}
			if r.Method == http.MethodGet {
				fiveHour := make([]map[string]int64, 0)
				if redemptions == 0 {
					fiveHour = append(fiveHour, map[string]int64{"expire_at": expiry.Unix()})
				}
				send(map[string]any{"available_five_hour_resets": fiveHour,
					"available_week_resets": []map[string]int64{{"expire_at": expiry.Add(time.Hour).UnixMilli()}}})
				return
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["idempotency_key"] != "attempt-1" ||
				payload["reset_type"] != "FIVE_HOUR" {
				t.Error("unexpected reset request")
			}
			redemptions++
			send(map[string]bool{"used": true})
		default:
			t.Errorf("unexpected upstream route: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	path := filepath.Join(t.TempDir(), "zcode", "account.json")
	service := New(path)
	service.baseURL = api.URL
	service.now = func() time.Time { return now }
	ctx := context.Background()

	login, err := service.Start(ctx)
	if err != nil || login.ID == "" || login.ID == "upstream-flow" || login.ExpiresAt != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("start: %#v, %v", login, err)
	}
	if status, err := service.Poll(ctx, "someone-else"); status != "expired" || err != nil {
		t.Fatalf("foreign flow: %s, %v", status, err)
	}
	if status, err := service.Poll(ctx, login.ID); status != "ready" || err != nil {
		t.Fatalf("poll: %s, %v", status, err)
	}
	service = New(path)
	service.baseURL = api.URL
	service.now = func() time.Time { return now }
	view, err := service.Account()
	if err != nil || !view.Connected || view.Account.DisplayName != "Plan owner" || view.SessionID == "" {
		t.Fatalf("saved account: %#v, %v", view, err)
	}
	data, _ := json.Marshal(view)
	if strings.Contains(string(data), "private") {
		t.Fatal("public account must not expose OAuth tokens")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("session storage must be private: %v, %v", info, err)
	}
	cards, err := service.Cards(ctx)
	if err != nil || len(cards.FiveHour) != 1 || cards.FiveHour[0] != expiry.UnixMilli() ||
		len(cards.Weekly) != 1 || cards.Weekly[0] != expiry.Add(time.Hour).UnixMilli() {
		t.Fatalf("cards: %#v, %v", cards, err)
	}
	use := UseRequest{SessionID: "old-session", ResetType: "FIVE_HOUR", ExpiresAt: expiry.UnixMilli(), AvailableCount: 1, IdempotencyKey: "attempt-1"}
	if err := service.Use(ctx, use); !errors.Is(err, ErrCardsChanged) {
		t.Fatalf("reject old session: %v", err)
	}
	use.SessionID = cards.SessionID
	if err := service.Use(ctx, use); err != nil {
		t.Fatalf("use: %v", err)
	}
	if err := service.Use(ctx, use); !errors.Is(err, ErrCardsChanged) {
		t.Fatalf("reject stale card: %v", err)
	}
	if redemptions != 1 {
		t.Fatalf("redemptions = %d", redemptions)
	}
	if err := service.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cards(ctx); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("disconnected: %v", err)
	}
}

func TestLoginRejectsNonProviderURL(t *testing.T) {
	for _, candidate := range []string{
		"https://chat.z.ai.evil.example/login", "https://chat.z.ai:8443/login",
		"http://chat.z.ai/login", "https://other@chat.z.ai/login",
	} {
		if validAuthorizationURL(candidate) {
			t.Errorf("accepted authorization URL %s", candidate)
		}
	}
	if !validAuthorizationURL("https://chat.z.ai/login") {
		t.Fatal("rejected Z.AI sign-in")
	}
}
