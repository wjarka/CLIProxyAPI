package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// RefreshReserveQuotas refreshes only protected accounts, using read-only usage
// endpoints and the provider executor's existing credential and proxy handling.
func (m *Manager) RefreshReserveQuotas(ctx context.Context) {
	m.refreshReserveQuotas(ctx, nil)
}

func (m *Manager) refreshReserveQuotas(ctx context.Context, providers []string) {
	if m == nil || !m.hasReservePolicies() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	refreshCtx := m.reserveRefreshCtx
	m.mu.RUnlock()
	if refreshCtx == nil || refreshCtx.Err() != nil {
		return
	}
	var results []<-chan singleflight.Result
	for _, auth := range m.List() {
		if auth.Disabled {
			continue
		}
		if !m.hasQuotaPolicyFor(auth) {
			continue
		}
		if len(providers) > 0 {
			matches := false
			for _, provider := range providers {
				if canonicalSchedulingProvider(provider) == canonicalSchedulingProvider(auth.Provider) {
					matches = true
					break
				}
			}
			if !matches {
				continue
			}
		}
		if m.quotaPolicyObservationFresh(m.reserveQuotaAuth(auth), time.Now()) {
			continue
		}
		result := m.reserveRefresh.DoChan(auth.ID, func() (any, error) {
			return nil, m.refreshOneReserveQuota(refreshCtx, auth.ID)
		})
		results = append(results, result)
	}
	if len(results) == 0 {
		return
	}
	// Bound only the wait during credential acquisition. Provider HTTP reads
	// continue in the manager lifetime without imposing an upstream timeout.
	// Stalled metadata can never hold unrelated inference or the dashboard.
	wait := time.NewTimer(500 * time.Millisecond)
	defer wait.Stop()
	for _, result := range results {
		select {
		case <-ctx.Done():
			return
		case <-wait.C:
			return
		case <-result:
			// Failed or incomplete reads leave the prior observation intact.
			// Never log response bodies, URLs with credentials, or token errors.
		}
	}
}

func (m *Manager) refreshOneReserveQuota(ctx context.Context, id string) error {
	now := time.Now()
	if retry, ok := m.reserveRefreshFailures.Load(id); ok && now.Before(retry.(time.Time)) {
		return nil
	}
	m.mu.RLock()
	current := m.auths[id]
	if current == nil {
		m.mu.RUnlock()
		return nil
	}
	auth := current.Clone()
	executor, ok := m.executorLocked(executorKeyFromAuth(auth))
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	if !m.hasQuotaPolicyFor(auth) {
		return nil
	}
	if m.quotaPolicyObservationFresh(m.reserveQuotaAuth(auth), now) {
		return nil
	}
	endpoint := ""
	switch strings.ToLower(auth.Provider) {
	case "claude":
		m.ReadClaudeUsage(ctx, id, ClaudeUsageQuota)
		return nil
	case "codex":
		endpoint = "https://chatgpt.com/backend-api/wham/usage"
	default:
		return nil
	}
	m.reserveRefreshFailures.Store(id, now.Add(15*time.Second))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if account, ok := auth.Metadata["account_id"].(string); ok && account != "" {
		req.Header.Set("Chatgpt-Account-Id", account)
	}
	resp, err := executor.HttpRequest(ctx, auth, req)
	if err != nil {
		return fmt.Errorf("reserve usage read failed")
	}
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("reserve usage read returned no body")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("reserve usage response close failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reserve usage read returned status %d", resp.StatusCode)
	}
	next, err := parseReserveUsage(auth.Provider, io.LimitReader(resp.Body, 1<<20), time.Now())
	if err != nil {
		return err
	}
	if !m.quotaPolicyObservationFresh(&Auth{ID: auth.ID, Provider: auth.Provider, Quota: next}, next.ObservedAt) {
		return fmt.Errorf("quota policy usage read has no valid observation for every required window")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current = m.auths[id]
	if current == nil || current.RegistrationEpoch != auth.RegistrationEpoch {
		return nil
	}
	// A newer inference response wins over a usage read started earlier.
	if newestReserveQuota(current).ObservedAt.After(now) {
		return nil
	}
	m.reserveQuotaObservations.Store(id, reserveQuotaObservation{epoch: auth.RegistrationEpoch, quota: next})
	m.reserveRefreshFailures.Delete(id)
	return nil
}

func parseReserveUsage(provider string, body io.Reader, observed time.Time) (QuotaState, error) {
	headers, err := reserveUsageHeaders(provider, body)
	if err != nil {
		return QuotaState{}, err
	}
	var quota QuotaState
	quota.ObserveResponseHeadersForProvider(provider, headers, observed)
	if _, remaining, _ := weeklyReserveWindow(&Auth{Provider: provider, Quota: quota}, observed); remaining == nil {
		return QuotaState{}, fmt.Errorf("reserve usage read has no valid weekly observation")
	}
	return quota, nil
}

func reserveUsageHeaders(provider string, body io.Reader) (http.Header, error) {
	headers := make(http.Header)
	if provider == "claude" {
		type window struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		// Usage responses also contain arrays, flags, and billing objects.
		// Decode only quota windows so unrelated provider fields cannot reject
		// an otherwise valid weekly observation.
		var usage struct {
			FiveHour *window `json:"five_hour"`
			SevenDay *window `json:"seven_day"`
			Fable    *window `json:"iguana_necktie"`
		}
		if err := json.NewDecoder(body).Decode(&usage); err != nil {
			return nil, fmt.Errorf("invalid reserve usage response")
		}
		for name, prefix := range map[string]string{"five_hour": "5h", "seven_day": "7d", "iguana_necktie": "7d_oi"} {
			w := map[string]*window{"five_hour": usage.FiveHour, "seven_day": usage.SevenDay, "iguana_necktie": usage.Fable}[name]
			if w == nil || w.Utilization == nil {
				continue
			}
			used := *w.Utilization
			if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
				continue
			}
			headers.Set("anthropic-ratelimit-unified-"+prefix+"-utilization", strconv.FormatFloat(used/100, 'f', -1, 64))
			headers.Set("anthropic-ratelimit-unified-"+prefix+"-reset", w.ResetsAt)
		}
	} else if provider == "codex" {
		type window struct {
			UsedPercent *float64 `json:"used_percent"`
			Seconds     int64    `json:"limit_window_seconds"`
			ResetAt     int64    `json:"reset_at"`
		}
		var usage struct {
			RateLimit struct {
				Primary   *window `json:"primary_window"`
				Secondary *window `json:"secondary_window"`
			} `json:"rate_limit"`
		}
		if err := json.NewDecoder(body).Decode(&usage); err != nil {
			return nil, fmt.Errorf("invalid reserve usage response")
		}
		for name, w := range map[string]*window{"primary": usage.RateLimit.Primary, "secondary": usage.RateLimit.Secondary} {
			if w == nil || w.UsedPercent == nil || w.Seconds <= 0 || w.Seconds%60 != 0 || w.ResetAt <= 0 {
				continue
			}
			used := *w.UsedPercent
			if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
				continue
			}
			prefix := "x-codex-" + name + "-"
			headers.Set(prefix+"used-percent", strconv.FormatFloat(used, 'f', -1, 64))
			headers.Set(prefix+"window-minutes", strconv.FormatInt(w.Seconds/60, 10))
			headers.Set(prefix+"reset-at", strconv.FormatInt(w.ResetAt, 10))
		}
	}
	return headers, nil
}
