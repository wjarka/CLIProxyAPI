package management

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type historyRoundTripper func(*http.Request) (*http.Response, error)

type historyClaudeExecutor struct {
	refreshRecordExecutor
	request func(context.Context, *http.Request) (*http.Response, error)
}

func (e *historyClaudeExecutor) HttpRequest(ctx context.Context, _ *coreauth.Auth, r *http.Request) (*http.Response, error) {
	return e.request(ctx, r)
}

func TestHistoryClaudeUsesManagementDeadlineAndOriginalCacheTime(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	ctx := coreauth.WithSkipPersist(context.Background())
	calls := 0
	m.RegisterExecutor(&historyClaudeExecutor{refreshRecordExecutor: refreshRecordExecutor{provider: "claude"}, request: func(ctx context.Context, r *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > defaultAPICallTimeout {
			t.Error("unbounded management quota read")
		}
		if r.URL.String() != coreauth.ClaudeUsageURL {
			t.Fatal("non-quota endpoint")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"seven_day":{"utilization":77,"resets_at":%q}}`, time.Now().Add(time.Hour).Format(time.RFC3339))))}, nil
	}})
	_, _ = m.Register(ctx, &coreauth.Auth{ID: "claude", Provider: "claude", Metadata: map[string]any{"account_uuid": "fixture", "organization_uuid": "fixture-org", "access_token": "fixture-token"}})
	h := NewHandler(&config.Config{}, "", m)
	source := newQuotaHistorySource(h)
	accounts, _ := source.Accounts(ctx)
	first, err := source.Fetch(ctx, accounts[0])
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	second, err := source.Fetch(ctx, accounts[0])
	if err != nil || len(second) != 1 || calls != 1 || !first[0].ObservedAt.Equal(second[0].ObservedAt) {
		t.Fatal("cache freshness changed", calls, err)
	}
}

func TestHistoryAntigravityPartialSuccessKeepsBackoff(t *testing.T) {
	for _, allFail := range []bool{false, true} {
		t.Run(fmt.Sprint(allFail), func(t *testing.T) {
			m := coreauth.NewManager(nil, nil, nil)
			ctx := coreauth.WithSkipPersist(context.Background())
			_, _ = m.Register(ctx, &coreauth.Auth{ID: "ag", Provider: "antigravity", Metadata: map[string]any{"access_token": "fixture", "project_id": "project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}})
			h := NewHandler(&config.Config{}, "", m)
			h.historyHTTPClient = &http.Client{Transport: historyRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("User-Agent") != "antigravity/hub/2.33.0 darwin/arm64" {
					t.Fatal("outdated identity")
				}
				if r.URL.String() == historyAntigravityURLs[0] {
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"3600"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				if allFail {
					return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.23,"resetTime":%q}]}]}`, time.Now().Add(time.Hour).Format(time.RFC3339))))}, nil
			})}
			source := newQuotaHistorySource(h)
			accounts, _ := source.Accounts(ctx)
			observations, err := source.Fetch(ctx, accounts[0])
			failure, ok := err.(*quotahistory.FetchError)
			expected := 1
			if allFail {
				expected = 0
			}
			if len(observations) != expected || !ok || time.Until(failure.RetryAt) < 59*time.Minute {
				t.Fatal("lost partial quota or cooldown", observations, err)
			}
		})
	}
}

func (f historyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHistorySourceIdentityConfiguredKeysAndReplacement(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	ctx := coreauth.WithSkipPersist(context.Background())
	for _, a := range []*coreauth.Auth{
		{ID: "a", Provider: "claude", Label: "A", Metadata: map[string]any{"account_uuid": "one", "organization_uuid": "org", "access_token": "secret-a"}},
		{ID: "b", Provider: "claude", Label: "B", Metadata: map[string]any{"account_uuid": "one", "organization_uuid": "org", "access_token": "secret-b"}},
		{ID: "z", Provider: "openai-compatible-zai-coding-plan", Attributes: map[string]string{"compat_name": "zai-coding-plan", "base_url": "https://api.z.ai/api/coding/paas/v4", "api_key": "secret-z"}},
		{ID: "d1", Provider: "devin", Metadata: map[string]any{"user_id": "seat1", "org_id": "org", "api_key": "seat-1"}},
		{ID: "d2", Provider: "devin", Metadata: map[string]any{"user_id": "seat2", "org_id": "org", "api_key": "seat-2"}},
	} {
		if _, err := m.Register(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(&config.Config{}, "", m)
	source := newQuotaHistorySource(h)
	accounts, err := source.Accounts(ctx)
	if err != nil || len(accounts) != 4 {
		t.Fatalf("%+v %v", accounts, err)
	}
	var old string
	for _, a := range accounts {
		if a.Provider == "claude" {
			old = a.ID
			if len(a.CredentialIDs) < 2 {
				t.Fatal(a)
			}
		}
	}
	_, _ = m.Register(ctx, &coreauth.Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"account_uuid": "two", "access_token": "replacement"}})
	accounts, _ = source.Accounts(ctx)
	if len(accounts) != 5 {
		t.Fatal("replacement joined former account")
	}
	found := false
	for _, a := range accounts {
		if a.ID == old {
			found = true
		}
	}
	if !found {
		t.Fatal("other alias history detached")
	}
}

func TestHistorySourceRejectsInvalidResponseAndDisableDuringFetch(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprint(disable), func(t *testing.T) {
			m := coreauth.NewManager(nil, nil, nil)
			ctx := coreauth.WithSkipPersist(context.Background())
			_, _ = m.Register(ctx, &coreauth.Auth{ID: "meta", Provider: "meta", Metadata: map[string]any{"dca_token": "dca:fixture"}})
			cfg := &config.Config{}
			h := NewHandler(cfg, "", m)
			h.historyHTTPClient = &http.Client{Transport: historyRoundTripper(func(*http.Request) (*http.Response, error) {
				body := "invalid"
				if disable {
					enabled := false
					h.mu.Lock()
					cfg.QuotaHistory.Enabled = &enabled
					h.mu.Unlock()
					body = `{}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			source := newQuotaHistorySource(h)
			accounts, _ := source.Accounts(ctx)
			if _, err := source.Fetch(ctx, accounts[0]); err == nil {
				t.Fatal("accepted invalid or disabled fetch")
			}
		})
	}
}

func TestHistorySourceReadOnlyMetaAndStaleCredential(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	ctx := coreauth.WithSkipPersist(context.Background())
	_, _ = m.Register(ctx, &coreauth.Auth{ID: "meta", Provider: "meta", Metadata: map[string]any{"dca_token": "dca:fixture", "api_key": "LLM-secret"}})
	h := NewHandler(&config.Config{}, "", m)
	h.historyHTTPClient = &http.Client{Transport: historyRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.meta.ai/muse-code/key" || r.Header.Get("Authorization") != "Bearer dca:fixture" {
			t.Fatalf("wrong quota credential/request %s", r.URL)
		}
		_, _ = m.Register(ctx, &coreauth.Auth{ID: "meta", Provider: "meta", Metadata: map[string]any{"dca_token": "dca:replacement"}})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"subs_usage":{"weekly":{"used_percent":77,"resets_at":%d}}}`, time.Now().Add(time.Hour).Unix())))}, nil
	})}
	source := newQuotaHistorySource(h)
	accounts, _ := source.Accounts(ctx)
	got, err := source.Fetch(ctx, accounts[0])
	if err == nil || len(got) > 0 {
		t.Fatal("replacement received old observation")
	}
}
