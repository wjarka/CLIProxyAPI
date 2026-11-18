package executor

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestClaudeUpstreamRetriesTemporaryDNSOnce(t *testing.T) {
	const payload = `{"model":"claude-fable-5-2"}`
	timedOut := &net.DNSError{Err: "i/o timeout", Name: "api.anthropic.com", IsTimeout: true}
	unavailable := &net.DNSError{Err: "server misbehaving", Name: "api.anthropic.com", IsTemporary: true}
	notFound := &net.DNSError{Err: "no such host", Name: "api.anthropic.com", IsNotFound: true}
	for _, tc := range []struct {
		name  string
		errs  []error
		calls int
		final error
	}{
		{name: "timeout", errs: []error{timedOut}, calls: 2},
		{name: "temporary resolver failure", errs: []error{unavailable}, calls: 2},
		{name: "repeated timeout", errs: []error{timedOut, unavailable}, calls: 2, final: unavailable},
		{name: "nonexistent host", errs: []error{notFound}, calls: 1, final: notFound},
		{name: "post-connection error", errs: []error{io.ErrUnexpectedEOF}, calls: 1, final: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				defer r.Body.Close()
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != payload || r.Header.Get("X-Api-Key") != "test-key" {
					t.Fatalf("attempt %d: payload or credential changed, read error = %v", calls, err)
				}
				if calls <= len(tc.errs) {
					return nil, fmt.Errorf("dial upstream: %w", tc.errs[calls-1])
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Api-Key", "test-key")

			resp, err := doClaudeUpstreamRequest(client, req)
			if resp != nil {
				defer resp.Body.Close()
			}
			if calls != tc.calls || !errors.Is(err, tc.final) {
				t.Fatalf("attempts = %d, error = %v, want %d attempts and %v", calls, err, tc.calls, tc.final)
			}
		})
	}
}
