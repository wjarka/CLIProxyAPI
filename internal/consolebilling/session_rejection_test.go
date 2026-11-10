package consolebilling

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeInvalidAuthorizationRequiresReconnect(t *testing.T) {
	for _, endpoint := range []string{"discovery", "credits"} {
		t.Run(endpoint, func(t *testing.T) {
			rejected := false
			m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
				discovery := r.URL.Path == "/api/organizations"
				if rejected && (discovery == (endpoint == "discovery")) {
					return response(403, `{"type":"error","error":{"type":"permission_error","message":"Invalid authorization"}}`)
				}
				if discovery {
					return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
				}
				return response(200, `{"amount":500}`)
			}))
			c, err := m.Connect(context.Background(), Claude, "primary", "fixture-session")
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Bind("credential", c.AccountIDs[0], true); err != nil {
				t.Fatal(err)
			}
			before := m.List()
			rejected = true
			err = m.Refresh(context.Background(), c.ID)
			if err == nil || err.Error() != "session_expired" {
				t.Fatalf("invalid authorization classified as %v", err)
			}
			after := m.List()
			if after.Connections[0].Status != "reconnect_required" || after.Connections[0].Error != "session_expired" {
				t.Fatalf("wrong reconnect state: %#v", after.Connections[0])
			}
			a := after.Accounts[0]
			if a.Status != "stale" || a.Error != "session_expired" || a.Balance == nil || *a.Balance != 5 || !reflect.DeepEqual(a.ObservedAt, before.Accounts[0].ObservedAt) || !reflect.DeepEqual(after.Bindings, before.Bindings) {
				t.Fatal("rejected session changed prior observation or bindings")
			}
		})
	}
}

func TestForbiddenBillingIsNotAlwaysSessionExpiry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider Provider
		body     string
	}{
		{"claude billing permission", Claude, `{"type":"error","error":{"type":"permission_error","message":"Not authorized to read billing"}}`},
		{"claude challenge", Claude, `<html>challenge</html>`},
		{"openai forbidden", OpenAI, `{"error":{"type":"permission_error","message":"Invalid authorization"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, fixtureClient(func(*http.Request) *http.Response { return response(403, tc.body) }))
			_, err := m.request(context.Background(), tc.provider, "fixture-session", "/fixture", "")
			if err == nil || err.Error() != "permission_denied" || strings.Contains(err.Error(), tc.body) {
				t.Fatalf("wrong safe error: %v", err)
			}
		})
	}
}
