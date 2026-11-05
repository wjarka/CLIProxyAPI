package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Codex app-server uses its integration's clientInfo.name as its HTTP identity.
// Treating it as a generic Responses client hides refusals behind EOF retries.
func TestForwardResponsesStreamAppServerPreservesRefusal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, originator, metadata, wantEvent string
	}{
		{"cezar", "cezar", `{"session_id":"session-1","thread_id":"thread-1"}`, "response.failed"},
		{"custom integration", "my_product", `{"session_id":"session-1","thread_id":"thread-1"}`, "response.failed"},
		{"generic client", "cezar", "", "error"},
		{"malformed metadata", "cezar", "{", "error"},
		{"empty metadata", "cezar", `{}`, "error"},
		{"missing session", "cezar", `{"thread_id":"thread-1"}`, "error"},
		{"missing thread", "cezar", `{"session_id":"session-1"}`, "error"},
		{"non-string IDs", "cezar", `{"session_id":1,"thread_id":2}`, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", tc.originator+"/0.164.0 (Ubuntu 24.4.0; x86_64) unknown ("+tc.originator+"; 0.1.0)")
			c.Request.Header.Set("Originator", tc.originator)
			c.Request.Header.Set("X-Codex-Turn-Metadata", tc.metadata)
			framer := &responsesSSEFramer{}
			framer.WriteChunk(c.Writer, []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n"))
			errs := make(chan *interfaces.ErrorMessage, 1)
			errs <- &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(`{"error":{"type":"invalid_request","code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk.","param":null}}`)}
			close(errs)
			h.forwardResponsesStream(c, c.Writer.(http.Flusher), func(error) {}, make(chan []byte), errs, framer)
			body := recorder.Body.String()
			if strings.Count(body, "event: "+tc.wantEvent+"\n") != 1 {
				t.Fatalf("expected one %s terminal event, got %s", tc.wantEvent, body)
			}
			if !strings.Contains(body, `"code":"cyber_policy"`) || !strings.Contains(body, "This content was flagged for possible cybersecurity risk.") {
				t.Fatalf("lost upstream refusal: %s", body)
			}
			if tc.wantEvent == "response.failed" && strings.Contains(body, "event: error\n") {
				t.Fatalf("Codex app-server received an unsupported error event: %s", body)
			}
		})
	}
}
