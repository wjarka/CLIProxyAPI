package consolebilling

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func fixtureClient(fn func(*http.Request) *http.Response) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return fn(r), nil })}
}
func claudeFixture(t *testing.T, credits string) *http.Client {
	t.Helper()
	return fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Scheme != "https" || r.URL.Host != "platform.claude.com" {
			t.Errorf("unsafe origin %s", r.URL)
		}
		if !strings.Contains(r.Header.Get("Cookie"), "sessionKey=") {
			t.Error("missing session cookie")
		}
		if r.URL.Path == "/api/organizations" {
			return response(200, `[{"uuid":"org-claude","name":"Console org","capabilities":["api"]},{"uuid":"chat","capabilities":["chat"]}]`)
		}
		if r.URL.Path == "/api/organizations/org-claude/prepaid/credits" {
			return response(200, credits)
		}
		t.Errorf("unexpected request %s", r.URL)
		return response(404, `{}`)
	})
}
func newTestManager(t *testing.T, c *http.Client) *Manager {
	t.Helper()
	m, e := NewManager(t.TempDir(), c)
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestClaudeMinorUnitsAndUnknownGrantFields(t *testing.T) {
	m := newTestManager(t, claudeFixture(t, `{"amount":12345,"currency":"USD","balance":{"credits":{"exponent":3}},"promo_tranches":[{"id":"grant-1","name":"Award","program_id":"subscription_api_credit","granted_amount_minor_units":20000,"remaining_amount_minor_units":12345,"expires_at":"2030-01-01T00:00:00Z"}],"tranches":[{"id":"grant-2","granted_amount_minor_units":1000} ]}`))
	c, e := m.Connect(context.Background(), Claude, "Primary", "fixture-session-one")
	if e != nil {
		t.Fatal(e)
	}
	s := m.List()
	if c.ID == "" || len(c.AccountIDs) != 1 || len(s.Accounts) != 1 {
		t.Fatalf("bad discovery %#v", s)
	}
	a := s.Accounts[0]
	if a.Balance == nil || *a.Balance != 12.345 || a.Currency != "USD" || a.Status != "ready" {
		t.Fatalf("bad balance %#v", a)
	}
	if len(a.Grants) != 2 || a.Grants[1].Remaining != nil || a.Grants[1].ExpiresAt != nil {
		t.Fatalf("invented grant fields %#v", a.Grants)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "fixture-session") {
		t.Fatal("snapshot leaks secret")
	}
}
func TestOpenAIDiscoveryZeroAndUnknown(t *testing.T) {
	for _, body := range []string{`{"total_available":0,"grants":{"data":[]}}`, `{"grants":{"data":[{"id":"expired","grant_amount":20,"used_amount":5,"expires_at":1}]}}`} {
		t.Run(body, func(t *testing.T) {
			m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
				if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer sess-fixture" {
					t.Errorf("wrong request %s", r.URL)
				}
				if r.URL.Path == "/v1/me" {
					return response(200, `{"orgs":{"data":[{"id":"org-openai","title":"OpenAI org"}]}}`)
				}
				if r.URL.Path != "/dashboard/billing/credit_grants" || r.Header.Get("OpenAI-Organization") != "org-openai" {
					t.Errorf("wrong billing request %s", r.URL)
				}
				return response(200, body)
			}))
			if _, e := m.Connect(context.Background(), OpenAI, "OpenAI", "sess-fixture"); e != nil {
				t.Fatal(e)
			}
			a := m.List().Accounts[0]
			if strings.Contains(body, "total_available") {
				if a.Balance == nil || *a.Balance != 0 || a.Status != "ready" {
					t.Fatal("zero lost")
				}
			} else {
				if a.Balance != nil || a.Status != "unknown" {
					t.Fatal("unknown became zero")
				}
				if a.Grants[0].Remaining != nil {
					t.Fatal("remaining inferred from amount-used")
				}
			}
		})
	}
}
func TestFailureSanitizationAndRedirects(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500, 302} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
				calls.Add(1)
				x := response(code, `{"secret":"fixture-secret"}`)
				x.Header.Set("Location", "https://attacker.invalid/?token=fixture-secret")
				return x
			}))
			if _, e := m.Connect(context.Background(), OpenAI, "x", "fixture-secret"); e == nil || strings.Contains(e.Error(), "fixture-secret") {
				t.Fatalf("unsafe error %v", e)
			}
			if calls.Load() != 1 || len(m.List().Connections) != 0 {
				t.Fatal("followed redirect or saved invalid session")
			}
		})
	}
}
func TestMalformedFinancialAmounts(t *testing.T) {
	for _, body := range []string{`{"amount":"100"}`, `{"amount":1.5}`, `{"amount":100,"currency":"EUR"}`, `{"amount":100,"balance":{"credits":{"exponent":5}}}`, `{"amount":100,"tranches":{}}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			m := newTestManager(t, claudeFixture(t, body))
			if _, e := m.Connect(context.Background(), Claude, "x", "session"); e == nil {
				t.Fatal("accepted malformed financial data")
			}
		})
	}
}
func TestPersistenceMultiLoginAndBindings(t *testing.T) {
	dir := t.TempDir()
	client := claudeFixture(t, `{"amount":1234}`)
	m, e := NewManager(dir, client)
	if e != nil {
		t.Fatal(e)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		c, e := m.Connect(context.Background(), Claude, fmt.Sprint(i), fmt.Sprintf("private-session-%d", i))
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, c.ID)
	}
	s := m.List()
	if len(s.Connections) != 3 || len(s.Accounts) != 1 {
		t.Fatalf("login/org identity conflated %#v", s)
	}
	if e = m.Bind("credential-one", s.Accounts[0].ID, true); e != nil {
		t.Fatal(e)
	}
	if e = m.Bind("credential-one", s.Accounts[0].ID, false); e != nil {
		t.Fatal(e)
	}
	if m.List().Bindings[0].Verified {
		t.Fatal("manual binding retained verification")
	}
	if e = m.Bind("credential-two", "absent", false); e == nil {
		t.Fatal("accepted missing account")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		b, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(b), "private-session") {
			t.Fatal("plaintext session on disk")
		}
		fi, _ := entry.Info()
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("unsafe mode %o", fi.Mode().Perm())
		}
	}
	restored, e := NewManager(dir, client)
	if e != nil {
		t.Fatal(e)
	}
	if len(restored.List().Connections) != 3 || len(restored.List().Bindings) != 1 {
		t.Fatal("restart lost data")
	}
	if e = restored.DeleteConnection(ids[0]); e != nil {
		t.Fatal(e)
	}
	if len(restored.List().Accounts) != 1 || len(restored.List().Bindings) != 1 {
		t.Fatal("disconnect deleted identity")
	}
}

func TestUnknownClaudeBalanceAndGrantExpirationArePreserved(t *testing.T) {
	m := newTestManager(t, claudeFixture(t, `{"amount":null,"tranches":[{"id":"expired","granted_amount_minor_units":100,"remaining_amount_minor_units":0,"expires_at":"2000-01-01T00:00:00Z"},{"id":"unknown-expiry","expires_at":"nonsense"}]}`))
	if _, e := m.Connect(context.Background(), Claude, "x", "fixture-session"); e != nil {
		t.Fatal(e)
	}
	a := m.List().Accounts[0]
	if a.Status != "unknown" || a.Balance != nil || len(a.Grants) != 2 || a.Grants[0].ExpiresAt == nil || a.Grants[0].ExpiresAt.Year() != 2000 || *a.Grants[0].Remaining != 0 || a.Grants[1].ExpiresAt != nil {
		t.Fatalf("unknown or expired grant metadata lost %#v", a)
	}
}
func TestOpenAIMultipleOrganizationsAreExplicitlyScoped(t *testing.T) {
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/v1/me" {
			return response(200, `{"orgs":{"data":[{"id":"org-one","title":"One"},{"id":"org-two","title":"Two"}]}}`)
		}
		if r.Header.Get("OpenAI-Organization") == "org-one" {
			return response(200, `{"total_available":4,"grants":{"data":[{"id":"g1","grant_amount":10,"remaining_amount":4,"expires_at":1893456000}]}}`)
		}
		if r.Header.Get("OpenAI-Organization") != "org-two" {
			t.Error("missing explicit organization scope")
		}
		return response(200, `{"total_available":8}`)
	}))
	c, e := m.Connect(context.Background(), OpenAI, "multi", "sess-fixture")
	if e != nil {
		t.Fatal(e)
	}
	s := m.List()
	if len(c.AccountIDs) != 2 || len(s.Accounts) != 2 || *s.Accounts[0].Balance != 4 || *s.Accounts[1].Balance != 8 {
		t.Fatalf("organizations conflated %#v", s)
	}
	g := s.Accounts[0].Grants[0]
	if *g.Amount != 10 || *g.Remaining != 4 || g.ExpiresAt == nil || g.ExpiresAt.Year() != 2030 {
		t.Fatal("grant amounts or Unix expiry lost")
	}
}
func TestBoundedResponsesUnsafeOrganizationsAndTransportErrors(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", maxResponseBytes+1), `{"orgs":{"data":[{"id":"../wrong"}]}}`, `{"orgs":{"data":[]}}`} {
		m := newTestManager(t, fixtureClient(func(*http.Request) *http.Response { return response(200, body) }))
		if _, e := m.Connect(context.Background(), OpenAI, "x", "fixture-private-token"); e == nil || strings.Contains(e.Error(), "fixture-private-token") {
			t.Fatalf("unsafe response accepted %v", e)
		}
	}
	m := newTestManager(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("https://attacker/?secret=fixture-private-token")
	})})
	if _, e := m.Connect(context.Background(), OpenAI, "x", "fixture-private-token"); e == nil || strings.Contains(e.Error(), "fixture-private-token") {
		t.Fatalf("transport error leaked %v", e)
	}
}
func TestUnsupportedEndpointFailsClearly(t *testing.T) {
	m := newTestManager(t, fixtureClient(func(*http.Request) *http.Response { return response(404, `{"raw":"private-token"}`) }))
	if _, e := m.Connect(context.Background(), OpenAI, "x", "private-token"); e == nil || e.Error() != "unsupported_endpoint" {
		t.Fatalf("unclear endpoint error %v", e)
	}
}

func TestOpenAIDashboardFallbackOnlyForUnsupportedEndpoint(t *testing.T) {
	for _, code := range []int{404, 405, 410, 401, 403, 429, 500, 302} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var paths []string
			client := fixtureClient(func(r *http.Request) *http.Response {
				if r.URL.Host != "api.openai.com" || r.URL.Scheme != "https" || r.Header.Get("Authorization") != "Bearer sess-fixture" {
					t.Errorf("unsafe fallback request %s", r.URL)
				}
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/v1/me" {
					return response(200, `{"orgs":{"data":[{"id":"org-scope"}]}}`)
				}
				if r.Header.Get("OpenAI-Organization") != "org-scope" {
					t.Error("billing fallback lost explicit organization")
				}
				if r.URL.Path == "/dashboard/billing/credit_grants" {
					return response(code, `{"raw":"private-fixture"}`)
				}
				if r.URL.Path != "/v1/dashboard/billing/credit_grants" {
					t.Errorf("unexpected fallback %s", r.URL)
				}
				return response(200, `{"total_available":3}`)
			})
			m := newTestManager(t, client)
			_, e := m.Connect(context.Background(), OpenAI, "fixture", "sess-fixture")
			fallback := code == 404 || code == 405 || code == 410
			if fallback {
				if e != nil || len(paths) != 3 || *m.List().Accounts[0].Balance != 3 {
					t.Fatalf("missing bounded fallback paths=%v err=%v", paths, e)
				}
			} else if e == nil || len(paths) != 2 {
				t.Fatalf("unexpected retry on HTTP %d paths=%v err=%v", code, paths, e)
			}
		})
	}
}
func TestOpenAIDashboardFallbackIsBounded(t *testing.T) {
	var billingCalls int
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/v1/me" {
			return response(200, `{"orgs":{"data":[{"id":"org-scope"}]}}`)
		}
		billingCalls++
		return response(404, `{}`)
	}))
	if _, e := m.Connect(context.Background(), OpenAI, "fixture", "sess-fixture"); e == nil || e.Error() != "unsupported_endpoint" || billingCalls != 2 {
		t.Fatalf("fallback not bounded calls=%d err=%v", billingCalls, e)
	}
}
