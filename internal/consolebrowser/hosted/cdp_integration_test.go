package hosted

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// This fixture speaks the real CDP transport; it never connects to a provider.
func cdpFixture(t *testing.T, provider string, exchange func(*websocket.Conn)) int {
	t.Helper()
	var port int
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/list" {
			_, _ = fmt.Fprintf(w, `[{"type":"page","url":"https://platform.%s.com/","webSocketDebuggerUrl":"ws://127.0.0.1:%d/devtools/page/one"}]`, provider, port)
			return
		}
		if r.URL.Path != "/devtools/page/one" {
			http.NotFound(w, r)
			return
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		exchange(ws)
	}))
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	port, _ = strconv.Atoi(p)
	s.Start()
	t.Cleanup(s.Close)
	return port
}
func TestCDPClaudeExtractsOnlyConsoleCookie(t *testing.T) {
	port := cdpFixture(t, "claude", func(ws *websocket.Conn) {
		var command struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				URLs []string `json:"urls"`
			} `json:"params"`
		}
		if err := ws.ReadJSON(&command); err != nil {
			t.Error(err)
			return
		}
		if command.Method != "Network.getCookies" || len(command.Params.URLs) != 2 || command.Params.URLs[0] != "https://claude.com/" || command.Params.URLs[1] != "https://platform.claude.com/" {
			t.Errorf("overbroad cookie request: %#v", command)
			return
		}
		_ = ws.WriteJSON(map[string]any{"id": command.ID, "result": map[string]any{"cookies": []map[string]string{{"domain": "accounts.google.com", "name": "sessionKey", "value": "google-secret"}, {"domain": ".claude.com", "name": "sessionKey", "value": "console-cookie"}}}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token, err := extractSession(ctx, port, "claude")
	if err != nil || token != "console-cookie" {
		t.Fatal(token, err)
	}
}
func TestCDPOpenAIObservesNetworkOnlyAfterFinishEnable(t *testing.T) {
	port := cdpFixture(t, "openai", func(ws *websocket.Conn) {
		var enable struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if ws.ReadJSON(&enable) != nil {
			return
		}
		if enable.Method != "Network.enable" {
			t.Error(enable.Method)
			return
		}
		_ = ws.WriteJSON(map[string]any{"id": enable.ID, "result": map[string]any{}})
		var reload struct {
			Method string `json:"method"`
		}
		if ws.ReadJSON(&reload) != nil {
			return
		}
		if reload.Method != "Page.reload" {
			t.Error(reload.Method)
			return
		}
		_ = ws.WriteJSON(map[string]any{"method": "Network.requestWillBeSentExtraInfo", "params": map[string]any{"requestId": "billing", "headers": map[string]string{"authorization": "Bearer dashboard-session"}}})
		_ = ws.WriteJSON(map[string]any{"method": "Network.requestWillBeSent", "params": map[string]any{"requestId": "billing", "request": map[string]any{"url": "https://api.openai.com/v1/dashboard/billing", "headers": map[string]any{}}}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token, err := extractSession(ctx, port, "openai")
	if err != nil || token != "dashboard-session" {
		t.Fatal(token, err)
	}
}
func TestCDPProviderErrorsAreOpaque(t *testing.T) {
	port := cdpFixture(t, "claude", func(ws *websocket.Conn) {
		var command json.RawMessage
		if ws.ReadJSON(&command) != nil {
			return
		}
		_ = ws.WriteJSON(map[string]any{"id": 1, "error": map[string]any{"message": "SECRET /profile/path accounts.google.com"}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token, err := extractSession(ctx, port, "claude")
	if token != "" || err != ErrNotReady {
		t.Fatal("raw provider error escaped", token, err)
	}
}
