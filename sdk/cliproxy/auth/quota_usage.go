package auth

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// QuotaUsageStatus describes the independent five-hour working-hours guard.
type QuotaUsageStatus struct {
	Policy           string                         `json:"policy"`
	Timezone         string                         `json:"timezone"`
	Schedule         []internalconfig.UsageSchedule `json:"schedule"`
	ProtectedPercent float64                        `json:"protected_percent"`
	Active           bool                           `json:"active"`
	ActiveUntil      time.Time                      `json:"active_until,omitempty"`
	UsedPercent      *float64                       `json:"used_percent,omitempty"`
	RemainingPercent *float64                       `json:"remaining_percent,omitempty"`
	ResetAt          time.Time                      `json:"reset_at,omitempty"`
	ObservedAt       time.Time                      `json:"observed_at,omitempty"`
	Blocked          bool                           `json:"blocked"`
	Reason           string                         `json:"reason,omitempty"`
}

func (m *Manager) usagePolicyFor(auth *Auth) (string, internalconfig.UsagePolicy, bool) {
	if m == nil || auth == nil {
		return "", internalconfig.UsagePolicy{}, false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg != nil {
		for _, binding := range cfg.Routing.AccountPolicies {
			if binding.Account == auth.ID {
				p, ok := cfg.Routing.UsagePolicies[binding.UsagePolicy]
				return binding.UsagePolicy, p, ok
			}
		}
	}
	return "", internalconfig.UsagePolicy{}, false
}

func fiveHourUsageWindow(auth *Auth, now time.Time) (reset time.Time, used *float64, observed time.Time) {
	quota := newestReserveQuota(auth)
	observed = quota.ObservedAt
	if observed.IsZero() || observed.After(now) || now.Sub(observed) > reserveObservationMaxAge {
		return
	}
	value := func(key string) string { return quota.Signals[http.CanonicalHeaderKey(key)] }
	raw, rawReset, scale := "", "", 1.0
	switch strings.ToLower(auth.Provider) {
	case "claude":
		raw = value("anthropic-ratelimit-unified-5h-utilization")
		rawReset = value("anthropic-ratelimit-unified-5h-reset")
		scale = 100
	case "codex":
		for _, window := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + window + "-"
			if value(prefix+"window-minutes") == "300" {
				raw = value(prefix + "used-percent")
				rawReset = value(prefix + "reset-at")
				if rawReset == "" {
					if seconds, err := strconv.ParseInt(value(prefix+"reset-after-seconds"), 10, 64); err == nil && seconds > 0 && seconds <= 5*60*60 {
						reset = observed.Add(time.Duration(seconds) * time.Second)
					}
				}
				break
			}
		}
	}
	if seconds, err := strconv.ParseInt(rawReset, 10, 64); err == nil && seconds > 0 {
		reset = time.Unix(seconds, 0)
	} else if parsed, err := time.Parse(time.RFC3339, rawReset); err == nil {
		reset = parsed
	}
	if !reset.After(now) {
		return reset, nil, observed
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	parsed *= scale
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 100 {
		return reset, nil, observed
	}
	return reset, &parsed, observed
}

// QuotaUsageStatus fails closed only inside protected hours. Weekly reserves
// remain independent and may still block the account outside this schedule.
func (m *Manager) QuotaUsageStatus(auth *Auth, now time.Time) *QuotaUsageStatus {
	name, p, ok := m.usagePolicyFor(auth)
	if !ok {
		return nil
	}
	status := &QuotaUsageStatus{Policy: name, Timezone: p.Timezone, Schedule: p.Schedule, ActiveUntil: p.ActiveUntil(now)}
	if p.ProtectedBudgetPercent != nil {
		status.ProtectedPercent = *p.ProtectedBudgetPercent
	}
	status.Active = !status.ActiveUntil.IsZero()
	status.ResetAt, status.UsedPercent, status.ObservedAt = fiveHourUsageWindow(m.reserveQuotaAuth(auth), now)
	if status.UsedPercent != nil {
		remaining := 100 - *status.UsedPercent
		status.RemainingPercent = &remaining
	}
	if !status.Active {
		return status
	}
	if status.RemainingPercent == nil || p.ProtectedBudgetPercent == nil {
		status.Blocked = true
		status.Reason = "quota_unknown"
		return status
	}
	// Provider utilization is scaled from a fraction; tolerate float rounding
	// so an observed value at the configured threshold always blocks.
	if status.ProtectedPercent > 0 && *status.RemainingPercent <= status.ProtectedPercent+1e-9 {
		status.Blocked = true
		status.Reason = "five_hour_limit"
	}
	return status
}

func (m *Manager) quotaPolicyBlockReason(auth *Auth, now time.Time) string {
	if reserve := m.QuotaReserveStatus(auth, now); reserve != nil && reserve.Blocked {
		return "quota_reserved"
	}
	if usage := m.QuotaUsageStatus(auth, now); usage != nil && usage.Blocked {
		return "quota_usage_guard"
	}
	return ""
}

func (m *Manager) hasQuotaPolicyFor(auth *Auth) bool {
	_, _, weekly := m.reservePolicyFor(auth)
	_, _, usage := m.usagePolicyFor(auth)
	return weekly || usage
}

// Every currently required window must be fresh. A valid weekly reading must
// not suppress refresh of a missing five-hour reading, or vice versa.
func (m *Manager) quotaPolicyObservationFresh(auth *Auth, now time.Time) bool {
	if _, _, ok := m.reservePolicyFor(auth); ok {
		if _, remaining, _ := weeklyReserveWindow(auth, now); remaining == nil {
			return false
		}
	}
	if _, p, ok := m.usagePolicyFor(auth); ok && !p.ActiveUntil(now).IsZero() {
		if _, used, _ := fiveHourUsageWindow(auth, now); used == nil {
			return false
		}
	}
	return true
}
