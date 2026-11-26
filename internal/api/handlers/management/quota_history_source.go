package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

var historyAntigravityURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

type quotaHistorySource struct{ h *Handler }

func newQuotaHistorySource(h *Handler) *quotaHistorySource { return &quotaHistorySource{h: h} }

func historyValue(a *coreauth.Auth, keys ...string) string {
	for _, k := range keys {
		if s, ok := a.Metadata[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if s := strings.TrimSpace(a.Attributes[k]); s != "" {
			return s
		}
	}
	return ""
}
func historyProvider(a *coreauth.Auth) string {
	p := strings.ToLower(a.Provider)
	if a.Attributes["compat_name"] == "zai-coding-plan" && strings.TrimRight(a.Attributes["base_url"], "/") == "https://api.z.ai/api/coding/paas/v4" {
		return "zai"
	}
	if p == "kimi-ai" {
		return "kimi"
	}
	switch p {
	case "claude", "codex", "antigravity", "devin", "kimi", "meta", "xai":
		return p
	}
	return ""
}
func historyAccountID(a *coreauth.Auth) string {
	p := historyProvider(a)
	claim := ""
	switch p {
	case "claude":
		if account := historyValue(a, "account_uuid"); account != "" {
			claim = account + ":" + historyValue(a, "organization_uuid")
		}
	case "codex":
		account, user := historyValue(a, "account_id", "chatgpt_account_id"), historyValue(a, "user_id", "chatgpt_user_id")
		if claims, err := codexauth.ParseJWTToken(historyValue(a, "id_token")); err == nil {
			if account == "" {
				account = claims.CodexAuthInfo.ChatgptAccountID
			}
			if user == "" {
				user = claims.CodexAuthInfo.ChatgptUserID
				if user == "" {
					user = claims.CodexAuthInfo.UserID
				}
				if user == "" {
					user = claims.Sub
				}
			}
		}
		// An organization id alone does not identify the seat's weekly allowance.
		if account != "" && user != "" {
			claim = account + ":" + user
		}
	case "devin":
		if user := historyValue(a, "user_id"); user != "" {
			claim = user + ":" + historyValue(a, "org_id")
		}
	default:
		claim = historyValue(a, "sub", "user_id", "account_uuid")
	}
	fallback := historyValue(a, "refresh_token", "dca_token", "session_token", "api_key", "access_token")
	if fallback == "" {
		fallback = a.ID + ":" + a.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return quotahistory.Identity(p, claim, fallback)
}

func (s *quotaHistorySource) manager() *coreauth.Manager {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.h.authManager
}
func (s *quotaHistorySource) Accounts(context.Context) ([]quotahistory.Account, error) {
	s.h.mu.Lock()
	enabled := historyEnabled(s.h.cfg)
	s.h.mu.Unlock()
	manager := s.manager()
	if manager == nil {
		return []quotahistory.Account{}, nil
	}
	entries := manager.List()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	groups := map[string]quotahistory.Account{}
	for _, a := range entries {
		provider := historyProvider(a)
		if provider == "" {
			continue
		}
		a.EnsureIndex()
		id := historyAccountID(a)
		disabled := !enabled || a.Disabled || a.Status == coreauth.StatusDisabled
		record, exists := groups[id]
		if !exists {
			label := a.Label
			if label == "" {
				label = a.FileName
			}
			if label == "" {
				label = provider
			}
			record = quotahistory.Account{ID: id, Provider: provider, Label: label, CredentialIDs: []string{}, Disabled: true}
		}
		// Auth indexes uniquely identify bundled and configured credentials. Physical
		// filenames must never join all seats in a bundled file to one quota pool.
		joins := []string{a.Index, a.ID}
		if provider != "devin" && provider != "zai" {
			joins = append(joins, a.FileName)
		}
		for _, join := range joins {
			if join != "" {
				found := false
				for _, v := range record.CredentialIDs {
					found = found || v == join
				}
				if !found {
					record.CredentialIDs = append(record.CredentialIDs, join)
				}
			}
		}
		record.Disabled = record.Disabled && disabled
		groups[id] = record
	}
	out := make([]quotahistory.Account, 0, len(groups))
	for _, a := range groups {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *quotaHistorySource) Fetch(ctx context.Context, account quotahistory.Account) ([]quotahistory.PoolObservation, error) {
	ctx, cancel := quotaHistoryReadContext(ctx)
	defer cancel()
	manager := s.manager()
	if manager == nil {
		return nil, &quotahistory.FetchError{Code: "credentials_unavailable"}
	}
	var selected *coreauth.Auth
	for _, a := range manager.List() {
		if !a.Disabled && a.Status != coreauth.StatusDisabled && historyProvider(a) == account.Provider && historyAccountID(a) == account.ID {
			if selected == nil || a.ID < selected.ID {
				selected = a
			}
		}
	}
	if selected == nil {
		return nil, &quotahistory.FetchError{Code: "credential_changed"}
	}
	epoch := selected.RegistrationEpoch
	s.h.mu.Lock()
	reader := &Handler{cfg: s.h.cfg.CloneForRuntime(), authManager: manager, tokenStore: s.h.tokenStore, historyHTTPClient: s.h.historyHTTPClient}
	s.h.mu.Unlock()
	var observations []quotahistory.PoolObservation
	var partialFailure *quotahistory.FetchError
	if account.Provider == "claude" {
		result := manager.ReadClaudeUsage(ctx, selected.ID, coreauth.ClaudeUsageQuota)
		if result.StatusCode != http.StatusOK {
			return nil, &quotahistory.FetchError{Code: "request_failed", RetryAt: result.RetryAt}
		}
		parsed, err := quotahistory.ParseWeekly("claude", account.ID, []byte(result.Body), result.ObservedAt)
		if err != nil {
			return nil, &quotahistory.FetchError{Code: "invalid_response"}
		}
		observations = parsed
	} else {
		token := historyValue(selected, "dca_token")
		if account.Provider != "meta" {
			var err error
			token, err = reader.resolveTokenForAuth(ctx, selected, "")
			if err != nil {
				return nil, &quotahistory.FetchError{Code: "credentials_unavailable"}
			}
		}
		if token == "" || (account.Provider == "meta" && !strings.HasPrefix(token, "dca:")) {
			return nil, &quotahistory.FetchError{Code: "credentials_unavailable"}
		}
		requests := historyQuotaRequests(selected, account.Provider, token)
		if len(requests) == 0 {
			return nil, &quotahistory.FetchError{Code: "missing_project"}
		}
		seen := map[string]bool{}
		var failure error
		success := false
		for _, r := range requests {
			body, header, status, err := reader.quotaHistoryRequest(ctx, selected, r.method, r.url, r.headers, r.body)
			if err != nil {
				failure = &quotahistory.FetchError{Code: "request_failed"}
				continue
			}
			if status < 200 || status >= 300 {
				code := "request_failed"
				if status == 429 {
					code = "rate_limited"
				}
				currentFailure := &quotahistory.FetchError{Code: code, RetryAt: historyRetryAt(header, time.Now())}
				failure = currentFailure
				if code == "rate_limited" || !currentFailure.RetryAt.IsZero() {
					if partialFailure == nil || currentFailure.RetryAt.After(partialFailure.RetryAt) {
						partialFailure = currentFailure
					}
				}
				continue
			}
			parsed, err := quotahistory.ParseWeekly(account.Provider, account.ID, body, time.Now().UTC())
			if err != nil {
				failure = &quotahistory.FetchError{Code: "invalid_response"}
				continue
			}
			success = true
			for _, o := range parsed {
				if !seen[o.PoolID] {
					seen[o.PoolID] = true
					observations = append(observations, o)
				}
			}
			if account.Provider != "antigravity" {
				break
			}
		}
		if !success && failure != nil {
			if partialFailure != nil {
				return nil, partialFailure
			}
			return nil, failure
		}
	}
	current, ok := manager.GetByID(selected.ID)
	s.h.mu.Lock()
	enabled := historyEnabled(s.h.cfg)
	s.h.mu.Unlock()
	if !enabled || !ok || current.Disabled || current.Status == coreauth.StatusDisabled || current.RegistrationEpoch != epoch || historyAccountID(current) != account.ID {
		return nil, &quotahistory.FetchError{Code: "credential_changed"}
	}
	if partialFailure != nil {
		return observations, partialFailure
	}
	return observations, nil
}

type historyRequest struct {
	method, url, body string
	headers           map[string]string
}

func historyQuotaRequests(a *coreauth.Auth, p, token string) []historyRequest {
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}
	r := historyRequest{method: http.MethodGet, headers: headers}
	switch p {
	case "codex":
		r.url = "https://chatgpt.com/backend-api/wham/usage"
		headers["User-Agent"] = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
		if id := historyValue(a, "account_id"); id != "" {
			headers["Chatgpt-Account-Id"] = id
		}
	case "kimi":
		r.url = "https://api.kimi.com/coding/v1/usages"
		domain := strings.ToLower(historyValue(a, "domain"))
		base, _ := url.Parse(historyValue(a, "base_url"))
		if domain == "ai" || domain == "kimi-ai" || domain == "kimi.ai" || strings.HasSuffix(domain, ".kimi.ai") || (domain == "" && base != nil && (base.Hostname() == "kimi.ai" || strings.HasSuffix(base.Hostname(), ".kimi.ai"))) {
			r.url = "https://api.kimi.ai/coding/v1/usages"
		}
	case "xai":
		r.url = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
		headers["x-xai-token-auth"] = "xai-grok-cli"
		headers["x-grok-client-version"] = "0.2.91"
		headers["User-Agent"] = "grok-pager/0.2.91 grok-shell/0.2.91 (macos; aarch64)"
		if id := historyValue(a, "sub", "user_id"); id != "" {
			headers["x-userid"] = id
		}
	case "zai":
		r.url = "https://api.z.ai/api/monitor/usage/quota/limit"
		headers["Accept-Language"] = "en-US,en"
	case "meta":
		r.method = http.MethodPost
		r.url = "https://api.meta.ai/muse-code/key"
		r.body = "{}"
		headers["Content-Type"] = "application/json"
		headers["x-api-version"] = "1.0.0"
	case "devin":
		r.method = http.MethodPost
		r.url = "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus"
		delete(headers, "Authorization")
		headers["Content-Type"] = "application/json"
		headers["Connect-Protocol-Version"] = "1"
		body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"ideName": "chisel", "ideVersion": "3000.10.21", "apiKey": token, "locale": "en", "os": "darwin", "extensionVersion": "3000.10.21", "clientName": "chisel"}})
		r.body = string(body)
	case "antigravity":
		project := historyValue(a, "project_id", "projectId", "gemini_virtual_project")
		if project == "" {
			project = fieldHistoryProject(a.Metadata)
		}
		if project == "" {
			return nil
		}
		body, _ := json.Marshal(map[string]string{"project": project})
		r.body = string(body)
		r.method = http.MethodPost
		headers["Content-Type"] = "application/json"
		headers["User-Agent"] = "antigravity/hub/2.33.0 darwin/arm64"
		out := []historyRequest{}
		for _, endpoint := range historyAntigravityURLs {
			copyRequest := r
			copyRequest.url = endpoint
			out = append(out, copyRequest)
		}
		return out
	default:
		return nil
	}
	return []historyRequest{r}
}
func mustHistoryJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func fieldHistoryProject(metadata map[string]any) string {
	raw := mustHistoryJSON(metadata)
	for _, key := range []string{"installed.project_id", "installed.projectId", "web.project_id", "web.projectId"} {
		if v := gjson.GetBytes(raw, key).String(); v != "" {
			return v
		}
	}
	return ""
}
func historyRetryAt(h http.Header, now time.Time) time.Time {
	value := h.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= 7*24*3600 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at
	}
	return time.Time{}
}
