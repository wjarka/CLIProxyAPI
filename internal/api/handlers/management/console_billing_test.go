package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type billingTestTransport func(*http.Request) (*http.Response, error)

func (f billingTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newBillingTestHandler(t *testing.T) *Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	m := coreauth.NewManager(nil, nil, nil)
	return NewHandler(&config.Config{}, filepath.Join(t.TempDir(), "config.yaml"), m)
}

func billingTestRequest(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/billing", h.GetConsoleBilling)
	router.POST("/billing/connections", h.ConnectConsoleBilling)
	router.PATCH("/billing/connections/:id", h.ReconnectConsoleBilling)
	router.DELETE("/billing/connections/:id", h.DeleteConsoleBilling)
	router.POST("/billing/connections/:id/refresh", h.RefreshConsoleBilling)
	router.PUT("/billing/bindings", h.BindConsoleBilling)
	router.DELETE("/billing/bindings", h.UnbindConsoleBilling)
	router.POST("/billing/credentials/verify", h.VerifyConsoleBillingCredential)
	router.GET("/billing/login-helper", h.DownloadConsoleLoginHelper)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestConsoleBillingListsOnlyFirstPartyAPIKeysWithoutSecrets(t *testing.T) {
	h := newBillingTestHandler(t)
	for _, a := range []*coreauth.Auth{
		{ID: "claude-api", Label: "Claude API", Provider: "claude", Attributes: map[string]string{"api_key": "never-show-claude"}},
		{ID: "openai-api", Label: "OpenAI API", Provider: "openai-compatibility:work", Attributes: map[string]string{"api_key": "never-show-openai", "base_url": "https://api.openai.com/v1", "compat_name": "work"}},
		{ID: "oauth", Provider: "claude", Metadata: map[string]any{"access_token": "never-show-oauth"}},
		{ID: "third-party", Provider: "claude", Attributes: map[string]string{"api_key": "never-show-other", "base_url": "https://example.org/v1"}},
	} {
		if _, err := h.authManager.Register(coreauth.WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	response := billingTestRequest(h, "GET", "/billing", "")
	if response.Code != 200 {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var got struct {
		Credentials                     []struct{ ID, Provider string }
		Connections, Accounts, Bindings []json.RawMessage
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Credentials) != 2 || got.Connections == nil || got.Accounts == nil || got.Bindings == nil {
		t.Fatalf("unexpected snapshot %s", response.Body.String())
	}
	for _, secret := range []string{"never-show", `"oauth"`, `"third-party"`, `"api_key"`, `"session"`} {
		if strings.Contains(response.Body.String(), secret) {
			t.Errorf("leaked %s", secret)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("billing data must not be cached")
	}
}

func TestConsoleBillingRejectsInvalidInputBeforeNetwork(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/billing/connections", `{"provider":"other","label":"X","session":"secret"}`},
		{"POST", "/billing/connections", `{"provider":"claude","label":"X","session":"secret","endpoint":"https://evil.test"}`},
		{"PUT", "/billing/bindings", `{"credential_id":"missing","account_id":"claude:x"}`},
		{"POST", "/billing/credentials/verify", `{"credential_id":"missing"}`},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			h := newBillingTestHandler(t)
			h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) { t.Fatal("unexpected network request"); return nil, nil })}
			response := billingTestRequest(h, tc.method, tc.path, tc.body)
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "evil.test") {
				t.Fatal("invalid request echoed")
			}
		})
	}
}

func TestConsoleBillingVerificationUsesReadOnlyFirstPartyOrigin(t *testing.T) {
	for _, provider := range []string{"claude", "openai"} {
		t.Run(provider, func(t *testing.T) {
			h := newBillingTestHandler(t)
			auth := &coreauth.Auth{ID: "key", Provider: "claude", Attributes: map[string]string{"api_key": "inference-only"}}
			wantHost := "api.anthropic.com"
			header := "anthropic-organization-id"
			if provider == "openai" {
				auth.Provider = "openai-compatibility:work"
				auth.Attributes["base_url"] = "https://api.openai.com/v1"
				wantHost = "api.openai.com"
				header = "openai-organization"
			}
			h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.Host != wantHost || r.URL.Path != "/v1/models" {
					t.Fatalf("unexpected probe %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Cookie") != "" {
					t.Fatal("Console session sent with inference verification")
				}
				if provider == "claude" && r.Header.Get("x-api-key") != "inference-only" {
					t.Fatal("missing API key")
				}
				if provider == "openai" && r.Header.Get("Authorization") != "Bearer inference-only" {
					t.Fatal("missing API key")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{http.CanonicalHeaderKey(header): []string{"org-correct"}}, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
			})}
			organization, err := h.verifyBillingOrganization(context.Background(), auth, provider)
			if err != nil || organization != "org-correct" {
				t.Fatalf("organization %q: %v", organization, err)
			}
		})
	}
}

func TestConsoleBillingVerificationRejectsRedirectsAndMissingOrganization(t *testing.T) {
	for _, status := range []int{200, 302, 401, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newBillingTestHandler(t)
			h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.anthropic.com" {
					t.Fatal("redirect followed with credentials")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://evil.test/"}}, Body: io.NopCloser(strings.NewReader("provider-echoed-secret"))}, nil
			})}
			_, err := h.verifyBillingOrganization(context.Background(), &coreauth.Auth{Attributes: map[string]string{"api_key": "secret"}}, "claude")
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "evil.test") {
				t.Fatalf("unsafe verification result %v", err)
			}
		})
	}
}

func TestConsoleBillingFirstPartyCredentialClassification(t *testing.T) {
	for _, tc := range []struct {
		provider, base string
		want           bool
	}{
		{"claude", "", true}, {"claude", "https://api.anthropic.com", true},
		{"claude", "https://api.anthropic.com.evil.test", false},
		{"claude", "https://api.anthropic.com:8443", false},
		{"claude", "https://api.anthropic.com/other", false},
		{"claude", "http://api.anthropic.com", false},
		{"claude", "https://user:password@api.anthropic.com", false},
		{"codex", "", false}, {"openai-compatibility:work", "https://api.openai.com/v1", true},
		{"openai-compatibility:work", "https://api.openai.com/v1?key=secret", false},
	} {
		t.Run(tc.provider+tc.base, func(t *testing.T) {
			_, ok := billingCredentialProvider(&coreauth.Auth{Provider: tc.provider, Attributes: map[string]string{"api_key": "key", "base_url": tc.base}})
			if ok != tc.want {
				t.Fatalf("eligible=%v want=%v", ok, tc.want)
			}
		})
	}
}

func TestConsoleBillingDownloadHelperContainsNoSecrets(t *testing.T) {
	response := billingTestRequest(newBillingTestHandler(t), "GET", "/billing/login-helper", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "--provider") || !strings.Contains(response.Body.String(), "getpass") {
		t.Fatalf("missing login helper: %d", response.Code)
	}
}
