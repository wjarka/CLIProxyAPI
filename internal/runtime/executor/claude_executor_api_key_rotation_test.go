package executor

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeAuthManagerRotatesOnAPIKeyBadRequest(t *testing.T) {
	const model = "claude-fable-5-2"
	const invalidRequest = `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid request"}}`
	const lowCredit = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`

	for _, tc := range []struct {
		name      string
		mode      string
		firstKind string
		errorBody string
		wantCalls []string
	}{
		{name: "stream out of credits", mode: "stream", firstKind: cliproxyauth.AuthKindAPIKey, errorBody: lowCredit, wantCalls: []string{"first", "second"}},
		{name: "non-stream bad request", firstKind: cliproxyauth.AuthKindAPIKey, errorBody: invalidRequest, wantCalls: []string{"first", "second"}},
		{name: "count tokens bad request", mode: "count", firstKind: cliproxyauth.AuthKindAPIKey, errorBody: invalidRequest, wantCalls: []string{"first", "second"}},
		{name: "OAuth bad request", firstKind: cliproxyauth.AuthKindOAuth, errorBody: invalidRequest, wantCalls: []string{"first"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
			for _, item := range []struct {
				name     string
				kind     string
				priority string
			}{
				{name: "first", kind: tc.firstKind, priority: "10"},
				{name: "second", kind: cliproxyauth.AuthKindAPIKey, priority: "1"},
			} {
				auth := &cliproxyauth.Auth{
					ID:       t.Name() + "/" + item.name,
					Provider: "claude",
					Attributes: map[string]string{
						cliproxyauth.AttributeAuthKind: item.kind,
						"priority":                     item.priority,
						"api_key":                      "test-key-" + item.name,
						"base_url":                     "https://api.anthropic.com",
					},
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				_ = r.Body.Close()
				name := strings.TrimPrefix(r.Header.Get("X-Api-Key"), "test-key-")
				calls = append(calls, name)
				code, body := http.StatusBadRequest, tc.errorBody
				switch {
				case name == "second" && tc.mode == "stream":
					code = http.StatusOK
					body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"model\":\"claude-fable-5-2\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
						"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				case name == "second" && tc.mode == "count":
					code, body = http.StatusOK, `{"input_tokens":1}`
				case name == "second":
					code, body = http.StatusOK, `{"id":"msg_ok","type":"message","model":"claude-fable-5-2","role":"assistant","content":[{"type":"text","text":"ok"}]}`
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"claude-fable-5-2","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			var err error
			switch tc.mode {
			case "stream":
				opts.Stream = true
				var stream *cliproxyexecutor.StreamResult
				stream, err = manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Error(chunk.Err)
						}
					}
				}
			case "count":
				_, err = manager.ExecuteCount(ctx, []string{"claude"}, req, opts)
			default:
				_, err = manager.Execute(ctx, []string{"claude"}, req, opts)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Fatalf("credentials tried = %v, want %v (error: %v)", calls, tc.wantCalls, err)
			}
			if (err == nil) != (len(tc.wantCalls) > 1) {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
