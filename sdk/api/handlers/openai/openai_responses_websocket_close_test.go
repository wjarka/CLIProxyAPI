package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

func TestResponsesWebsocketTerminalErrorsUseCloseFrames(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		message     string
		wantPayload bool
		wantClose   int
	}{
		{
			name:      "upstream overloaded",
			status:    http.StatusBadGateway,
			message:   `{"error":{"code":"server_is_overloaded","message":"Please try again later"}}`,
			wantClose: websocket.CloseTryAgainLater,
		},
		{
			name:      "credentials cooling down",
			status:    http.StatusServiceUnavailable,
			message:   "auth_unavailable: no auth available",
			wantClose: websocket.CloseTryAgainLater,
		},
		{
			name:        "invalid request",
			status:      http.StatusBadRequest,
			message:     `{"error":{"type":"invalid_request_error","message":"Invalid input"}}`,
			wantPayload: true,
			wantClose:   websocket.CloseNormalClosure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverErr := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					serverErr <- errUpgrade
					return
				}
				_, _, errWrite := writeResponsesWebsocketTerminalError(newResponsesWebsocketWriter(conn), nil, &interfaces.ErrorMessage{
					StatusCode: tt.status,
					Error:      errors.New(tt.message),
				}, nil)
				serverErr <- errWrite
			}))
			defer server.Close()
			conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if errDial != nil {
				t.Fatalf("dial websocket: %v", errDial)
			}
			defer func() { _ = conn.Close() }()
			if errDeadline := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
				t.Fatalf("set read deadline: %v", errDeadline)
			}
			if tt.wantPayload {
				_, payload, errRead := conn.ReadMessage()
				if errRead != nil || !strings.Contains(string(payload), "Invalid input") {
					t.Fatalf("expected request error payload, got %s, %v", payload, errRead)
				}
			}
			_, payload, errRead := conn.ReadMessage()
			var closeErr *websocket.CloseError
			if !errors.As(errRead, &closeErr) || closeErr.Code != tt.wantClose || closeErr.Text != "" {
				t.Fatalf("expected close %d without internal detail, got payload=%s error=%v", tt.wantClose, payload, errRead)
			}
			if errServer := <-serverErr; !errors.Is(errServer, websocket.ErrCloseSent) {
				t.Fatalf("terminal close error: %v", errServer)
			}
		})
	}
}
