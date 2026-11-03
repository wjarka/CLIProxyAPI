package auth

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const reserveObservationMaxAge = time.Minute

type reserveQuotaObservation struct {
	epoch uint64
	quota QuotaState
}

// Active reserve reads have their own snapshot. They must not replace unrelated
// passive observations or mark old plan/credit/model data as freshly observed.
func (m *Manager) reserveQuotaAuth(auth *Auth) *Auth {
	if auth == nil {
		return nil
	}
	value, ok := m.reserveQuotaObservations.Load(auth.ID)
	if !ok {
		return auth
	}
	observation := value.(reserveQuotaObservation)
	if observation.epoch != auth.RegistrationEpoch || !observation.quota.ObservedAt.After(newestReserveQuota(auth).ObservedAt) {
		return auth
	}
	copy := auth.Clone()
	copy.Quota.ObservedAt = observation.quota.ObservedAt
	copy.Quota.Signals = observation.quota.Signals
	return copy
}

// QuotaReserveStatus is a read-only view of the account's automation reserve.
// Direct provider usage is not restricted or attributed to individual clients.
type QuotaReserveStatus struct {
	Policy           string    `json:"policy"`
	ProtectedPercent float64   `json:"protected_percent"`
	RemainingPercent *float64  `json:"remaining_percent,omitempty"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	NextReleaseAt    time.Time `json:"next_release_at,omitempty"`
	ObservedAt       time.Time `json:"observed_at,omitempty"`
	Blocked          bool      `json:"blocked"`
	Reason           string    `json:"reason,omitempty"`
}

func (m *Manager) hasReservePolicies() bool {
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && len(cfg.Routing.AccountPolicies) > 0
}

func (m *Manager) reservePolicyFor(auth *Auth) (string, internalconfig.ReservePolicy, bool) {
	if m == nil || auth == nil {
		return "", internalconfig.ReservePolicy{}, false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg != nil {
		for _, binding := range cfg.Routing.AccountPolicies {
			if binding.Account == auth.ID {
				policy, ok := cfg.Routing.ReservePolicies[binding.ReservePolicy]
				return binding.ReservePolicy, policy, ok
			}
		}
	}
	return "", internalconfig.ReservePolicy{}, false
}

func newestReserveQuota(auth *Auth) QuotaState {
	quota := auth.Quota
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.ObservedAt.After(quota.ObservedAt) {
			quota = state.Quota
		}
	}
	return quota
}

// weeklyReserveWindow always uses the shared weekly pool, including for Fable.
// A model-specific weekly allowance must not hide depletion of the shared pool.
func weeklyReserveWindow(auth *Auth, now time.Time) (reset time.Time, remaining *float64, observed time.Time) {
	quota := newestReserveQuota(auth)
	observed = quota.ObservedAt
	reset = WeeklyQuotaReset(&Auth{Provider: auth.Provider, Quota: quota}, "", now)
	if reset.IsZero() || observed.IsZero() || observed.After(now) || now.Sub(observed) > reserveObservationMaxAge {
		return reset, nil, observed
	}
	value := func(key string) string { return quota.Signals[http.CanonicalHeaderKey(key)] }
	raw, scale := "", 1.0
	switch strings.ToLower(auth.Provider) {
	case "claude":
		raw, scale = value("anthropic-ratelimit-unified-7d-utilization"), 100
	case "codex":
		for _, window := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + window + "-"
			if value(prefix+"window-minutes") == "10080" {
				raw = value(prefix + "used-percent")
				break
			}
		}
	}
	used, err := strconv.ParseFloat(raw, 64)
	used *= scale
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return reset, nil, observed
	}
	balance := 100 - used
	return reset, &balance, observed
}

// QuotaReserveStatus returns nil for unrestricted accounts. Missing or stale
// observations block protected accounts until a read-only usage refresh succeeds.
func (m *Manager) QuotaReserveStatus(auth *Auth, now time.Time) *QuotaReserveStatus {
	name, policy, ok := m.reservePolicyFor(auth)
	if !ok {
		return nil
	}
	reset, remaining, observed := weeklyReserveWindow(m.reserveQuotaAuth(auth), now)
	status := &QuotaReserveStatus{Policy: name, RemainingPercent: remaining, ResetAt: reset, ObservedAt: observed}
	if reset.IsZero() {
		status.Blocked = true
		status.Reason = "quota_unknown"
		return status
	}
	status.ProtectedPercent, status.NextReleaseAt = policy.ProtectedReserve(now, reset)
	if remaining == nil {
		status.Blocked = true
		status.Reason = "quota_unknown"
		return status
	}
	if status.ProtectedPercent == 0 {
		return status
	}
	if *remaining <= status.ProtectedPercent+1e-9 {
		status.Blocked = true
		status.Reason = "quota_reserved"
	}
	return status
}
