package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	ClaudeUsageURL        = "https://api.anthropic.com/api/oauth/usage"
	ClaudeResetGrantsURL  = ClaudeUsageURL + "?cedar_ember=1&skip_spend=1"
	claudeUsageMinBackoff = 2 * time.Minute
	claudeUsageMaxBackoff = 30 * time.Minute
)

type ClaudeUsageKind int

const (
	ClaudeUsageQuota ClaudeUsageKind = iota
	ClaudeUsageGrantStatus
	ClaudeUsageGrantPreflight
)

// ClaudeUsageResult is a successful usage snapshot or a sanitized upstream error.
// A stale result is display-only. Reserve routing uses independently dated observations.
type ClaudeUsageResult struct {
	StatusCode int
	Body       string
	ObservedAt time.Time
	RetryAt    time.Time
	Stale      bool
	freshUntil time.Time
}

type claudeUsageAccount struct {
	epoch       uint64
	gate        chan struct{}
	invalidated bool

	usage, grants           ClaudeUsageResult
	usageError, grantsError ClaudeUsageResult
	retryAt                 time.Time
	backoff                 time.Duration
	limitedGrants           bool
}

// ReadClaudeUsage shares the usage snapshot with reserve routing. Grant status
// has its own response shape but shares the credential's throttle.
func (m *Manager) ReadClaudeUsage(ctx context.Context, id string, kind ClaudeUsageKind) ClaudeUsageResult {
	grants := kind != ClaudeUsageQuota
	m.mu.Lock()
	auth := m.auths[id]
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || !strings.EqualFold(auth.Provider, "claude") {
		m.mu.Unlock()
		return ClaudeUsageResult{StatusCode: http.StatusNotFound}
	}
	value, _ := m.claudeUsage.Load(id)
	state, _ := value.(*claudeUsageAccount)
	if state == nil || state.epoch != auth.RegistrationEpoch {
		state = &claudeUsageAccount{epoch: auth.RegistrationEpoch, gate: make(chan struct{}, 1)}
		m.claudeUsage.Store(id, state)
	}
	m.mu.Unlock()

	// A reserve probe can run for the manager lifetime. Bound the time other
	// readers wait for its cache slot without imposing an upstream deadline.
	select {
	case state.gate <- struct{}{}:
	default:
		wait := time.NewTimer(2 * time.Second)
		defer wait.Stop()
		select {
		case state.gate <- struct{}{}:
		case <-wait.C:
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.currentClaudeUsageAuthLocked(id, state) != nil && !grants && state.usage.Body != "" {
				result := state.usage
				result.Stale = true
				return result
			}
			return ClaudeUsageResult{StatusCode: http.StatusServiceUnavailable}
		case <-ctx.Done():
			return ClaudeUsageResult{StatusCode: http.StatusBadGateway}
		}
	}
	defer func() { <-state.gate }()

	m.mu.Lock()
	auth = m.currentClaudeUsageAuthLocked(id, state)
	if auth == nil {
		m.mu.Unlock()
		return ClaudeUsageResult{StatusCode: http.StatusNotFound}
	}
	now := time.Now()
	snapshot, failure, endpoint := &state.usage, &state.usageError, ClaudeUsageURL
	if grants {
		snapshot, failure, endpoint = &state.grants, &state.grantsError, ClaudeResetGrantsURL
	}
	if state.invalidated {
		state.invalidated = false
		state.usage.freshUntil = time.Time{}
		state.grants.freshUntil = time.Time{}
	}
	if kind != ClaudeUsageGrantPreflight && now.Before(snapshot.freshUntil) {
		result := *snapshot
		m.mu.Unlock()
		return result
	}
	if now.Before(state.retryAt) {
		result := throttledClaudeUsage(*snapshot, grants, state.retryAt)
		m.mu.Unlock()
		return result
	}
	if now.Before(failure.RetryAt) {
		result := *failure
		m.mu.Unlock()
		return result
	}
	auth = auth.Clone()
	executor, ok := m.executorLocked(executorKeyFromAuth(auth))
	m.mu.Unlock()
	if !ok {
		return ClaudeUsageResult{StatusCode: http.StatusBadGateway}
	}

	result, retryAfter := fetchClaudeUsage(ctx, executor, auth, endpoint)
	observed := time.Now()
	var quota QuotaState
	if !grants && result.StatusCode == http.StatusOK {
		quota, _ = parseReserveUsage("claude", strings.NewReader(result.Body), observed)
	}
	// Do not let a response from a removed or re-registered credential escape.
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.currentClaudeUsageAuthLocked(id, state)
	if current == nil {
		return ClaudeUsageResult{StatusCode: http.StatusNotFound}
	}
	if result.StatusCode == http.StatusTooManyRequests {
		state.backoff = min(max(2*state.backoff, claudeUsageMinBackoff), claudeUsageMaxBackoff)
		state.retryAt = maxClaudeUsageRetry(observed, state.backoff, retryAfter)
		state.limitedGrants = grants
		return throttledClaudeUsage(*snapshot, grants, state.retryAt)
	}
	if result.StatusCode != http.StatusOK {
		result.RetryAt = observed.Add(15 * time.Second)
		*failure = result
		return result
	}
	if state.invalidated {
		return ClaudeUsageResult{StatusCode: http.StatusServiceUnavailable}
	}
	if state.limitedGrants == grants {
		state.retryAt, state.backoff = time.Time{}, 0
	}
	*failure = ClaudeUsageResult{}
	result.ObservedAt = observed
	result.freshUntil = observed.Add(reserveObservationMaxAge)
	if !quota.ObservedAt.IsZero() {
		reset := WeeklyQuotaReset(&Auth{Provider: "claude", Quota: quota}, "", observed)
		if reset.Before(result.freshUntil) {
			result.freshUntil = reset
		}
		if !newestReserveQuota(current).ObservedAt.After(now) {
			m.reserveQuotaObservations.Store(id, reserveQuotaObservation{epoch: state.epoch, quota: quota})
		}
	}
	*snapshot = result
	return result
}

func (m *Manager) currentClaudeUsageAuthLocked(id string, state *claudeUsageAccount) *Auth {
	current := m.auths[id]
	active, _ := m.claudeUsage.Load(id)
	if current == nil || active != state || current.RegistrationEpoch != state.epoch ||
		current.Disabled || current.Status == StatusDisabled || !strings.EqualFold(current.Provider, "claude") {
		return nil
	}
	return current
}

// InvalidateClaudeUsage expires snapshots after a quota reset while preserving
// the last known usage and any active upstream throttle.
func (m *Manager) InvalidateClaudeUsage(auth *Auth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if value, ok := m.claudeUsage.Load(auth.ID); ok {
		state := value.(*claudeUsageAccount)
		if current := m.currentClaudeUsageAuthLocked(auth.ID, state); current != nil &&
			current.RegistrationEpoch == auth.RegistrationEpoch && !CredentialsChanged(auth, current) {
			state.invalidated = true
			m.reserveQuotaObservations.Delete(auth.ID)
		}
	}
}

func throttledClaudeUsage(snapshot ClaudeUsageResult, grants bool, retryAt time.Time) ClaudeUsageResult {
	if !grants && snapshot.Body != "" {
		snapshot.Stale = true
		snapshot.RetryAt = retryAt
		return snapshot
	}
	return ClaudeUsageResult{StatusCode: http.StatusTooManyRequests, RetryAt: retryAt}
}

func fetchClaudeUsage(ctx context.Context, executor ProviderExecutor, auth *Auth, endpoint string) (ClaudeUsageResult, string) {
	failed := ClaudeUsageResult{StatusCode: http.StatusBadGateway}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return failed, ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	resp, err := executor.HttpRequest(ctx, auth, req)
	if err != nil || resp == nil || resp.Body == nil {
		return failed, ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ClaudeUsageResult{StatusCode: resp.StatusCode}, resp.Header.Get("Retry-After")
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if trimmed := bytes.TrimSpace(body); err != nil || len(body) > maxBody || len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return failed, ""
	}
	return ClaudeUsageResult{StatusCode: http.StatusOK, Body: string(body)}, ""
}

func maxClaudeUsageRetry(now time.Time, delay time.Duration, retryAfter string) time.Time {
	after := strings.TrimSpace(retryAfter)
	retry := now.Add(delay)
	if seconds, err := strconv.ParseInt(after, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		if upstream := now.Add(time.Duration(seconds) * time.Second); upstream.After(retry) {
			return upstream
		}
	} else if upstream, err := http.ParseTime(after); err == nil && upstream.After(retry) {
		return upstream
	}
	return retry
}
