package consolebilling

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Source contracts:
// Claude: https://github.com/steipete/CodexBar/blob/main/docs/claudeplatform.md
// and Sources/CodexBarCore/Resources/Plugins/claudeplatform.ts (minor units/tranches).
// OpenAI discovery: https://help.openai.com/en/articles/9132009-how-can-i-view-the-users-or-organizations-associated-with-an-api-key
// Dashboard credits: https://github.com/GPIOX/dsh-api-balance/blob/main/lib/index.js
// Browser-session compatibility paths: https://github.com/yeestrada/Copilot_Desktop_Monitor/blob/main/src/openai_api.py
// Only an unsupported endpoint triggers the single same-origin /v1/dashboard fallback.
// /v1/me discovery still requires dashboard-token compatibility, which is not live-validated.
// These are read-only requests. Dashboard-session compatibility is not an official API guarantee;
// unsupported discovery or billing returns an explicit error rather than guessing an organization.
const maxResponseBytes = 1 << 20

var organizationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type discovery struct {
	accounts []Account
	session  string
}

func normalizeSession(p Provider, s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 32768 || strings.ContainsAny(s, "\r\n\x00") {
		return "", failure("invalid_session")
	}
	switch p {
	case Claude:
		s = strings.TrimSpace(strings.TrimPrefix(s, "Cookie:"))
		if strings.Contains(s, "=") {
			req := &http.Request{Header: http.Header{"Cookie": []string{s}}}
			for _, cookie := range req.Cookies() {
				if cookie.Name == "sessionKey" && cookie.Value != "" {
					return "sessionKey=" + cookie.Value, nil
				}
			}
			return "", failure("invalid_session")
		}
		if strings.ContainsAny(s, "; \t") {
			return "", failure("invalid_session")
		}
		return "sessionKey=" + s, nil
	case OpenAI:
		s = strings.TrimSpace(strings.TrimPrefix(s, "Bearer "))
		if strings.ContainsAny(s, " \t") || strings.HasPrefix(s, "sk-") {
			return "", failure("invalid_session")
		}
		return s, nil
	default:
		return "", failure("invalid_provider")
	}
}
func (m *Manager) request(ctx context.Context, p Provider, session, path, org string) ([]byte, error) {
	origin := "https://api.openai.com"
	if p == Claude {
		origin = "https://platform.claude.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+path, nil)
	if err != nil {
		return nil, failure("invalid_response")
	}
	req.Header.Set("Accept", "application/json")
	if p == Claude {
		req.Header.Set("Cookie", session)
	} else {
		req.Header.Set("Authorization", "Bearer "+session)
		req.Header.Set("Origin", "https://platform.openai.com")
		if org != "" {
			req.Header.Set("OpenAI-Organization", org)
		}
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, failure("provider_unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case 200:
	case 401:
		return nil, failure("session_expired")
	case 403:
		// Claude also reports revoked Console sessions as a permission_error.
		// Only its explicit authentication rejection means reconnect; a billing
		// permission denial must remain distinct from an invalid session.
		if p == Claude {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
			var rejection struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if readErr == nil && len(body) <= maxResponseBytes && json.Unmarshal(body, &rejection) == nil &&
				(rejection.Error.Type == "permission_error" || rejection.Error.Type == "authentication_error") &&
				strings.EqualFold(strings.TrimSpace(rejection.Error.Message), "Invalid authorization") {
				return nil, failure("session_expired")
			}
		}
		return nil, failure("permission_denied")
	case 404, 405, 410:
		return nil, failure("unsupported_endpoint")
	case 429:
		return nil, failure("rate_limited")
	default:
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, failure("redirect_rejected")
		}
		return nil, failure("provider_unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, failure("invalid_response")
	}
	return body, nil
}
func (m *Manager) discover(ctx context.Context, p Provider, raw string) (discovery, error) {
	session, err := normalizeSession(p, raw)
	if err != nil {
		return discovery{}, err
	}
	var accounts []Account
	var lastBillingErr error
	if p == Claude {
		body, err := m.request(ctx, p, session, "/api/organizations", "")
		if err != nil {
			return discovery{}, err
		}
		var orgs []struct {
			UUID         string   `json:"uuid"`
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
		}
		if json.Unmarshal(body, &orgs) != nil || orgs == nil {
			return discovery{}, failure("invalid_response")
		}
		for _, org := range orgs {
			api := false
			for _, c := range org.Capabilities {
				if c == "api" {
					api = true
				}
			}
			if !api {
				continue
			}
			if !organizationPattern.MatchString(org.UUID) {
				return discovery{}, failure("invalid_response")
			}
			b, err := m.request(ctx, p, session, "/api/organizations/"+org.UUID+"/prepaid/credits", "")
			var a Account
			if err == nil {
				a, err = parseClaude(b)
			}
			if err != nil {
				if err.Error() == "session_expired" {
					return discovery{}, err
				}
				lastBillingErr = err
				a = Account{Currency: "USD", Grants: []Grant{}, Error: err.Error()}
			}
			a.OrganizationID = org.UUID
			a.Name = org.Name
			accounts = append(accounts, a)
		}
	} else {
		body, err := m.request(ctx, p, session, "/v1/me", "")
		if err != nil {
			return discovery{}, err
		}
		var me struct {
			Orgs struct {
				Data []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"data"`
			} `json:"orgs"`
		}
		if json.Unmarshal(body, &me) != nil || me.Orgs.Data == nil {
			return discovery{}, failure("invalid_response")
		}
		for _, org := range me.Orgs.Data {
			if !organizationPattern.MatchString(org.ID) {
				return discovery{}, failure("invalid_response")
			}
			b, err := m.request(ctx, p, session, "/dashboard/billing/credit_grants", org.ID)
			if err != nil && err.Error() == "unsupported_endpoint" {
				b, err = m.request(ctx, p, session, "/v1/dashboard/billing/credit_grants", org.ID)
			}
			var a Account
			if err == nil {
				a, err = parseOpenAI(b)
			}
			if err != nil {
				if err.Error() == "session_expired" {
					return discovery{}, err
				}
				lastBillingErr = err
				a = Account{Currency: "USD", Grants: []Grant{}, Error: err.Error()}
			}
			a.OrganizationID = org.ID
			a.Name = org.Title
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return discovery{}, failure("no_organizations")
	}
	readable := false
	for _, a := range accounts {
		if a.Error == "" {
			readable = true
		}
	}
	if !readable {
		return discovery{}, lastBillingErr
	}
	seen := make(map[string]bool)
	now := time.Now().UTC()
	for i := range accounts {
		a := &accounts[i]
		if seen[a.OrganizationID] {
			return discovery{}, failure("invalid_response")
		}
		seen[a.OrganizationID] = true
		a.Provider = p
		a.ID = string(p) + ":" + a.OrganizationID
		a.Status = "ready"
		if a.Balance == nil {
			a.Status = "unknown"
		}
		if a.Error == "" {
			a.ObservedAt = &now
		} else {
			a.Status = "unknown"
		}
		if a.Grants == nil {
			a.Grants = []Grant{}
		}
		// Organization names are display metadata, not raw provider error messages.
		if len(a.Name) > 512 || strings.Contains(a.Name, raw) || strings.Contains(a.Name, session) {
			a.Name = a.OrganizationID
		}
	}
	return discovery{accounts: accounts, session: session}, nil
}
func object(body []byte) (map[string]json.RawMessage, error) {
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil || v == nil {
		return nil, failure("invalid_response")
	}
	return v, nil
}
func number(raw json.RawMessage, minor bool) (*float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 9007199254740991 || (minor && math.Trunc(n) != n) {
		return nil, failure("invalid_response")
	}
	return &n, nil
}
func currency(v map[string]json.RawMessage) (string, error) {
	c := "USD"
	if raw := v["currency"]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &c) != nil {
			return "", failure("invalid_response")
		}
	}
	if strings.ToUpper(c) != "USD" {
		return "", failure("unsupported_currency")
	}
	return "USD", nil
}
func textField(v map[string]json.RawMessage, k string) (string, error) {
	var s string
	if raw := v[k]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &s) != nil || len(s) > 1024 {
			return "", failure("invalid_response")
		}
	}
	return s, nil
}
func expiry(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, e := time.Parse(time.RFC3339, s); e == nil {
			v = v.UTC()
			return &v
		}
		return nil
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 && n < 253402300800 && math.Trunc(n) == n {
		v := time.Unix(int64(n), 0).UTC()
		return &v
	}
	return nil
}
func parseClaude(body []byte) (Account, error) {
	v, err := object(body)
	if err != nil {
		return Account{}, err
	}
	if _, ok := v["amount"]; !ok {
		return Account{}, failure("invalid_response")
	}
	c, err := currency(v)
	if err != nil {
		return Account{}, err
	}
	exponent := 2
	if raw := v["balance"]; len(raw) > 0 && string(raw) != "null" {
		var b struct {
			Credits struct {
				Exponent *int `json:"exponent"`
			} `json:"credits"`
		}
		if json.Unmarshal(raw, &b) != nil {
			return Account{}, failure("invalid_response")
		}
		if b.Credits.Exponent != nil {
			exponent = *b.Credits.Exponent
		}
		if exponent < 0 || exponent > 4 {
			return Account{}, failure("invalid_response")
		}
	}
	amount, err := number(v["amount"], true)
	if err != nil {
		return Account{}, err
	}
	scale := math.Pow10(exponent)
	if amount != nil {
		*amount /= scale
	}
	a := Account{Currency: c, Balance: amount, Grants: []Grant{}}
	for _, kind := range []string{"promo_tranches", "tranches"} {
		raw := v[kind]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var rows []json.RawMessage
		if json.Unmarshal(raw, &rows) != nil {
			return Account{}, failure("invalid_response")
		}
		for _, row := range rows {
			g, err := object(row)
			if err != nil {
				return Account{}, err
			}
			id, err := textField(g, "id")
			if err != nil {
				return Account{}, err
			}
			name, err := textField(g, "name")
			if err != nil {
				return Account{}, err
			}
			program, err := textField(g, "program_id")
			if err != nil {
				return Account{}, err
			}
			granted, err := number(g["granted_amount_minor_units"], true)
			if err != nil {
				return Account{}, err
			}
			remaining, err := number(g["remaining_amount_minor_units"], true)
			if err != nil {
				return Account{}, err
			}
			if granted != nil {
				if *granted < 0 {
					return Account{}, failure("invalid_response")
				}
				*granted /= scale
			}
			if remaining != nil {
				if *remaining < 0 {
					return Account{}, failure("invalid_response")
				}
				*remaining /= scale
			}
			if program == "" {
				program = kind
			}
			a.Grants = append(a.Grants, Grant{ID: id, Name: name, Kind: program, Amount: granted, Remaining: remaining, ExpiresAt: expiry(g["expires_at"])})
		}
	}
	return a, nil
}
func parseOpenAI(body []byte) (Account, error) {
	v, err := object(body)
	if err != nil {
		return Account{}, err
	}
	if _, available := v["total_available"]; !available {
		if _, grants := v["grants"]; !grants {
			return Account{}, failure("invalid_response")
		}
	}
	c, err := currency(v)
	if err != nil {
		return Account{}, err
	}
	amount, err := number(v["total_available"], false)
	if err != nil {
		return Account{}, err
	}
	a := Account{Currency: c, Balance: amount, Grants: []Grant{}}
	raw := v["grants"]
	if len(raw) == 0 || string(raw) == "null" {
		return a, nil
	}
	g, err := object(raw)
	if err != nil {
		return Account{}, err
	}
	data := g["data"]
	if len(data) == 0 || string(data) == "null" {
		return Account{}, failure("invalid_response")
	}
	var rows []json.RawMessage
	if json.Unmarshal(data, &rows) != nil {
		return Account{}, failure("invalid_response")
	}
	for _, row := range rows {
		v, err := object(row)
		if err != nil {
			return Account{}, err
		}
		if _, err = currency(v); err != nil {
			return Account{}, err
		}
		id, err := textField(v, "id")
		if err != nil {
			return Account{}, err
		}
		name, err := textField(v, "name")
		if err != nil {
			return Account{}, err
		}
		kind, err := textField(v, "type")
		if err != nil {
			return Account{}, err
		}
		amount, err := number(v["grant_amount"], false)
		if err != nil {
			return Account{}, err
		}
		remaining, err := number(v["remaining_amount"], false)
		if err != nil {
			return Account{}, err
		}
		if (amount != nil && *amount < 0) || (remaining != nil && *remaining < 0) {
			return Account{}, failure("invalid_response")
		}
		a.Grants = append(a.Grants, Grant{ID: id, Name: name, Kind: kind, Amount: amount, Remaining: remaining, ExpiresAt: expiry(v["expires_at"])})
	}
	return a, nil
}
