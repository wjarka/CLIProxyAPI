package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestConsoleBillingHTTPLifecycleAndOrganizationIdentity(t *testing.T) {
	h := newBillingTestHandler(t)
	probeOrg := "org-openai"
	changedDuringProbe := false
	h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		headers := make(http.Header)
		switch r.URL.Host + r.URL.Path {
		case "platform.claude.com/api/organizations":
			body = `[{"uuid":"org-claude","name":"Claude org","capabilities":["api"]}]`
		case "platform.claude.com/api/organizations/org-claude/prepaid/credits":
			body = `{"amount":1234,"currency":"USD"}`
		case "api.openai.com/v1/me":
			body = `{"orgs":{"data":[{"id":"org-openai","title":"OpenAI org"}]}}`
		case "api.openai.com/dashboard/billing/credit_grants":
			if r.Header.Get("OpenAI-Organization") != "org-openai" {
				t.Fatal("unscoped balance request")
			}
			body = `{"total_available":42,"grants":{"data":[]}}`
		case "api.openai.com/v1/models":
			headers.Set("OpenAI-Organization", probeOrg)
			if changedDuringProbe {
				current, _ := h.authManager.GetByID("openai-key")
				current.Attributes["header:OpenAI-Project"] = "proj-new"
				if _, err := h.authManager.Update(coreauth.WithSkipPersist(context.Background()), current); err != nil {
					t.Fatal(err)
				}
			}
		default:
			t.Fatalf("unexpected URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, auth := range []*coreauth.Auth{
		{ID: "claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "claude-inference-secret"}},
		{ID: "openai-key", Provider: "openai-compatibility:work", Attributes: map[string]string{"api_key": "openai-inference-secret", "base_url": "https://api.openai.com/v1"}},
	} {
		if _, err := h.authManager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatal(err)
		}
	}
	request := func(method, path, body string, want int) string {
		t.Helper()
		r := billingTestRequest(h, method, path, body)
		if r.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, r.Code, want, r.Body.String())
		}
		if strings.Contains(r.Body.String(), "secret") || strings.Contains(r.Body.String(), "verification_fingerprint") {
			t.Fatal("response leaked private credential material")
		}
		return r.Body.String()
	}
	ids := map[string]string{}
	for _, provider := range []string{"claude", "openai"} {
		result := request("POST", "/billing/connections", `{"provider":"`+provider+`","label":"Personal","session":"sess-console-secret"}`, 200)
		var parsed struct {
			Connection consolebilling.Connection `json:"connection"`
		}
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			t.Fatal(err)
		}
		ids[provider] = parsed.Connection.ID
	}
	request("PUT", "/billing/bindings", `{"credential_id":"claude-key","account_id":"openai:org-openai"}`, 400)
	request("PUT", "/billing/bindings", `{"credential_id":"openai-key","account_id":"openai:org-openai"}`, 200)
	assertVerified := func(want bool) {
		t.Helper()
		var snapshot struct {
			Bindings    []consolebilling.Binding   `json:"bindings"`
			Credentials []consoleBillingCredential `json:"credentials"`
		}
		if err := json.Unmarshal([]byte(request("GET", "/billing", "", 200)), &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Bindings) != 1 || snapshot.Bindings[0].Verified != want {
			t.Fatalf("verification snapshot %+v want %v", snapshot.Bindings, want)
		}
		for _, cred := range snapshot.Credentials {
			if cred.ID == "openai-key" && (cred.VerificationStatus == "verified") != want {
				t.Fatalf("credential status %+v", cred)
			}
		}
	}
	assertVerified(false)
	request("POST", "/billing/credentials/verify", `{"credential_id":"openai-key"}`, 200)
	assertVerified(true)
	request("POST", "/billing/connections/"+ids["openai"]+"/refresh", `{}`, 200)
	request("DELETE", "/billing/connections/"+ids["openai"], "", 200)
	assertVerified(true) // identity survives billing-session disconnection
	if !strings.Contains(request("GET", "/billing", "", 200), "reconnect_required") {
		t.Fatal("missing disconnected state")
	}
	request("PATCH", "/billing/connections/"+ids["openai"], `{"session":"sess-reconnected-secret"}`, 200)
	request("PUT", "/billing/bindings", `{"credential_id":"openai-key","account_id":"openai:org-openai"}`, 200)
	assertVerified(false) // manual rebinding never inherits verification
	probeOrg = "org-not-connected"
	request("POST", "/billing/credentials/verify", `{"credential_id":"openai-key"}`, 409)
	probeOrg = "org-openai"
	request("POST", "/billing/credentials/verify", `{"credential_id":"openai-key"}`, 200)
	current, _ := h.authManager.GetByID("openai-key")
	current.Attributes["header:OpenAI-Project"] = "proj-other"
	if _, err := h.authManager.Update(coreauth.WithSkipPersist(context.Background()), current); err != nil {
		t.Fatal(err)
	}
	assertVerified(false) // same ID, changed organization/project configuration
	changedDuringProbe = true
	request("POST", "/billing/credentials/verify", `{"credential_id":"openai-key"}`, 409)
	request("DELETE", "/billing/bindings?credential_id=openai-key", "", 200)
}

func TestConsoleBillingVerificationHonorsFixedOrganizationSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, org, project, echo string
		wantRequest, wantOK      bool
	}{
		{"fixed", "org-B", "proj-B", "org-B", true, true},
		{"mismatch", "org-B", "proj-B", "org-A", true, false},
		{"dynamic-org", "$ORG", "", "org-B", false, false},
		{"dynamic-project", "org-B", "$PROJECT", "org-B", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBillingTestHandler(t)
			called := false
			h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.Header.Get("OpenAI-Organization") != tc.org || r.Header.Get("OpenAI-Project") != tc.project {
					t.Fatal("configured selectors missing")
				}
				if r.Header.Get("X-Unrelated") != "" {
					t.Fatal("arbitrary custom header leaked")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Openai-Organization": []string{tc.echo}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}
			_, err := h.verifyBillingOrganization(context.Background(), &coreauth.Auth{Attributes: map[string]string{"api_key": "key", "header:OpenAI-Organization": tc.org, "header:OpenAI-Project": tc.project, "header:X-Unrelated": "private"}}, "openai")
			if called != tc.wantRequest || (err == nil) != tc.wantOK {
				t.Fatalf("called=%v error=%v", called, err)
			}
		})
	}
}

func TestConsoleBillingSafeErrorsStayActionableWithoutLoggingOut(t *testing.T) {
	h := newBillingTestHandler(t)
	h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private-session-secret"))}, nil
	})}
	r := billingTestRequest(h, "POST", "/billing/connections", `{"provider":"openai","label":"Work","session":"sess-private"}`)
	if r.Code != 502 || !strings.Contains(r.Body.String(), `"session_expired"`) || strings.Contains(r.Body.String(), "secret") {
		t.Fatalf("unsafe or unhelpful error %d %s", r.Code, r.Body.String())
	}
}
