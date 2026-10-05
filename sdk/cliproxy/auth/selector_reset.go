package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// EarliestResetSelector spends the eligible account whose weekly window ends
// first. Availability, priority and websocket capability remain authoritative;
// SessionAffinitySelector can wrap it to keep established conversations sticky.
type EarliestResetSelector struct {
	fallback RoundRobinSelector
	now      func() time.Time
}

func (s *EarliestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var selected *Auth
	var earliest time.Time
	for _, candidate := range available {
		reset := WeeklyQuotaReset(candidate, model, now)
		if !reset.IsZero() && (selected == nil || reset.Before(earliest)) {
			selected, earliest = candidate, reset
		}
	}
	if selected != nil {
		return selected, nil
	}
	return s.fallback.Pick(ctx, provider, model, opts, available)
}

// WeeklyQuotaReset returns a future reset from the newest passive observation.
// Unknown, expired and old snapshots deliberately do not influence routing.
// This is a scheduling hint, never a replacement for upstream cooldown handling.
func WeeklyQuotaReset(auth *Auth, model string, now time.Time) time.Time {
	if auth == nil {
		return time.Time{}
	}
	quota := auth.Quota
	if state := auth.ModelStates[canonicalModelKey(model)]; state != nil && state.Quota.ObservedAt.After(quota.ObservedAt) {
		quota = state.Quota
	}
	if quota.ObservedAt.IsZero() || quota.ObservedAt.After(now) || now.Sub(quota.ObservedAt) > 24*time.Hour {
		return time.Time{}
	}
	value := func(key string) string {
		return quota.Signals[http.CanonicalHeaderKey(key)]
	}
	parse := func(raw string) time.Time {
		var result time.Time
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			result = time.Unix(seconds, 0)
		} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			result = parsed
		}
		if !result.After(now) {
			return time.Time{}
		}
		return result
	}
	switch strings.ToLower(auth.Provider) {
	case "claude":
		if strings.Contains(strings.ToLower(model), "fable") {
			if reset := parse(value("anthropic-ratelimit-unified-7d_oi-reset")); !reset.IsZero() {
				return reset
			}
		}
		return parse(value("anthropic-ratelimit-unified-7d-reset"))
	case "codex":
		// Window labels alone are insufficient: primary/secondary can change.
		for _, window := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + window + "-"
			if value(prefix+"window-minutes") != "10080" {
				continue
			}
			if reset := parse(value(prefix + "reset-at")); !reset.IsZero() {
				return reset
			}
			if seconds, err := strconv.ParseInt(value(prefix+"reset-after-seconds"), 10, 64); err == nil && seconds > 0 && seconds <= 7*24*60*60 {
				reset := quota.ObservedAt.Add(time.Duration(seconds) * time.Second)
				if reset.After(now) {
					return reset
				}
			}
		}
	}
	return time.Time{}
}
