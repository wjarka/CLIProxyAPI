package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ConsoleRoutingGrant struct {
	Remaining      float64
	RemainingKnown bool
	ExpiresAt      time.Time
}
type ConsoleRoutingObservation struct {
	Provider              string
	AccountID             string
	CredentialFingerprint string
	ObservedAt            time.Time
	ValidUntil            time.Time
	Grants                []ConsoleRoutingGrant
}
type ConsoleRoutingSnapshot map[string]ConsoleRoutingObservation

type consoleRoutingContextKey struct{}

const consoleRoutingLCPGuardKey = "console_routing_lcp_guard"

// SetConsoleRoutingSnapshot publishes a detached immutable scheduling view.
func (m *Manager) SetConsoleRoutingSnapshot(snapshot ConsoleRoutingSnapshot) {
	copySnapshot := make(ConsoleRoutingSnapshot, len(snapshot))
	for id, observation := range snapshot {
		observation.Grants = append([]ConsoleRoutingGrant(nil), observation.Grants...)
		copySnapshot[id] = observation
	}
	m.consoleRouting.Store(&copySnapshot)
}
func (m *Manager) consoleRoutingContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if snapshot := m.consoleRouting.Load(); snapshot != nil {
		return context.WithValue(ctx, consoleRoutingContextKey{}, *snapshot)
	}
	return ctx
}
func BillingCredentialProvider(auth *Auth) (string, bool) {
	if auth == nil || strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return "", false
	}
	raw := strings.TrimSpace(auth.Attributes["base_url"])
	if strings.EqualFold(auth.Provider, "claude") && officialBillingBase(raw, "api.anthropic.com", true) {
		return "claude", true
	}
	if raw != "" && officialBillingBase(raw, "api.openai.com", false) {
		return "openai", true
	}
	return "", false
}
func officialBillingBase(raw, host string, allowEmpty bool) bool {
	if raw == "" {
		return allowEmpty
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, host) || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "" || path == "/v1"
}
func BillingCredentialFingerprint(auth *Auth, provider string) string {
	if auth == nil {
		return ""
	}
	values := []string{provider, auth.Provider, auth.Attributes["api_key"], auth.Attributes["base_url"]}
	var headers []string
	for key, value := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			headers = append(headers, key+"="+value)
		}
	}
	sort.Strings(headers)
	values = append(values, headers...)
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
func consoleObservation(ctx context.Context, auth *Auth) (ConsoleRoutingObservation, bool) {
	if ctx == nil {
		return ConsoleRoutingObservation{}, false
	}
	snapshot, _ := ctx.Value(consoleRoutingContextKey{}).(ConsoleRoutingSnapshot)
	if auth == nil {
		return ConsoleRoutingObservation{}, false
	}
	observation, ok := snapshot[auth.ID]
	provider, valid := BillingCredentialProvider(auth)
	return observation, ok && valid && observation.Provider == provider && observation.AccountID != "" && observation.CredentialFingerprint != "" && observation.CredentialFingerprint == BillingCredentialFingerprint(auth, provider)
}
func consoleGrantReset(observation ConsoleRoutingObservation, now time.Time) time.Time {
	if observation.ObservedAt.IsZero() || observation.ObservedAt.After(now) || now.Sub(observation.ObservedAt) > 5*time.Minute || observation.ValidUntil.IsZero() || !observation.ValidUntil.After(now) {
		return time.Time{}
	}
	var earliest time.Time
	for _, g := range observation.Grants {
		if !g.RemainingKnown || math.IsNaN(g.Remaining) || math.IsInf(g.Remaining, 0) || g.Remaining <= 0 || !g.ExpiresAt.After(now) {
			continue
		}
		if earliest.IsZero() || g.ExpiresAt.Before(earliest) {
			earliest = g.ExpiresAt
		}
	}
	return earliest
}

// consoleOrganizationResets shares the best current observation among verified keys of an organization.
func consoleOrganizationResets(ctx context.Context, auths []*Auth, now time.Time) map[string]time.Time {
	resets := make(map[string]time.Time)
	for _, a := range auths {
		if o, ok := consoleObservation(ctx, a); ok {
			reset := consoleGrantReset(o, now)
			if old := resets[o.AccountID]; !reset.IsZero() && (old.IsZero() || reset.Before(old)) {
				resets[o.AccountID] = reset
			}
		}
	}
	return resets
}
func selectorReset(ctx context.Context, auth *Auth, model string, now time.Time, resets map[string]time.Time) time.Time {
	if o, ok := consoleObservation(ctx, auth); ok {
		return resets[o.AccountID]
	}
	return WeeklyQuotaReset(auth, model, now)
}

// keepConsoleAffinity limits reranking to verified API bindings under earliest-reset.
func (s *SessionAffinitySelector) keepConsoleAffinity(ctx context.Context, bound *Auth, available []*Auth, provider, model string, now time.Time) bool {
	if _, ok := s.fallback.(*EarliestResetSelector); !ok {
		return true
	}
	if _, ok := consoleObservation(ctx, bound); !ok {
		return true
	}
	highest := preferCodexWebsocketAuths(ctx, provider, highestPriorityAuths(available))
	found := false
	for _, a := range highest {
		if a.ID == bound.ID {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	resets := consoleOrganizationResets(ctx, highest, now)
	boundReset := selectorReset(ctx, bound, model, now, resets)
	for _, a := range highest {
		reset := selectorReset(ctx, a, model, now, resets)
		if !reset.IsZero() && (boundReset.IsZero() || reset.Before(boundReset)) {
			return false
		}
	}
	return true
}
