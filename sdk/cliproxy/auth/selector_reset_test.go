package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetTestAuth(id string, now time.Time, delay time.Duration) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Quota: QuotaState{
		ObservedAt: now,
		Signals:    map[string]string{http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-reset"): strconv.FormatInt(now.Add(delay).Unix(), 10)},
	}}
}

func TestEarliestResetAvailabilityAndFallback(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a, b := resetTestAuth("a", now, 3*24*time.Hour), resetTestAuth("b", now, time.Hour)
	s := &EarliestResetSelector{now: func() time.Time { return now }}
	pick := func(want string) {
		t.Helper()
		got, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{a, b})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v, %v; want %s", got, err, want)
		}
	}
	pick("b")
	b.Disabled = true
	pick("a")
	b.Disabled = false
	a.Attributes = map[string]string{"priority": "10"}
	pick("a")
	a.Attributes = nil
	b.Quota.Exceeded = true
	b.Quota.Reason = "credential_quota"
	b.Quota.NextRecoverAt = now.Add(time.Hour)
	pick("a")
	b.Quota = QuotaState{}
	a.Quota = QuotaState{}
	pick("a")
	pick("b")
}

func TestWeeklyQuotaResetParsing(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name, provider, model string
		age                   time.Duration
		signals               map[string]string
		want                  time.Time
	}{
		{"claude", "claude", "claude-opus-5.5", 0, map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, now.Add(time.Hour)},
		{"fable bucket", "claude", "claude-fable-5.1", 0, map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10), "anthropic-ratelimit-unified-7d_oi-reset": strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)}, now.Add(2 * time.Hour)},
		{"codex absolute", "codex", "gpt", 0, map[string]string{"x-codex-secondary-window-minutes": "10080", "x-codex-secondary-reset-at": now.Add(time.Hour).Format(time.RFC3339)}, now.Add(time.Hour)},
		{"codex relative", "codex", "gpt", time.Minute, map[string]string{"x-codex-primary-window-minutes": "10080", "x-codex-primary-reset-after-seconds": "3600"}, now.Add(59 * time.Minute)},
		{"not weekly", "codex", "gpt", 0, map[string]string{"x-codex-primary-window-minutes": "300", "x-codex-primary-reset-after-seconds": "3600"}, time.Time{}},
		{"expired", "claude", "", 0, map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(now.Unix(), 10)}, time.Time{}},
		{"stale", "claude", "", 25 * time.Hour, map[string]string{"anthropic-ratelimit-unified-7d-reset": strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}, time.Time{}},
		{"malformed", "claude", "", 0, map[string]string{"anthropic-ratelimit-unified-7d-reset": "not a date"}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{Provider: tc.provider, Quota: QuotaState{ObservedAt: now.Add(-tc.age), Signals: map[string]string{}}}
			for k, v := range tc.signals {
				a.Quota.Signals[http.CanonicalHeaderKey(k)] = v
			}
			if got := WeeklyQuotaReset(a, tc.model, now); !got.Equal(tc.want) {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestEarliestResetAffinitySurvivesRankingChange(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a, b := resetTestAuth("a", now, time.Hour), resetTestAuth("b", now, 2*time.Hour)
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &EarliestResetSelector{}, TTL: time.Hour})
	defer s.Stop()
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"thread-1"}}}
	// Use the explicit Codex/Claude-compatible session header recognized upstream.
	opts.Headers.Set("X-Session-Id", "thread-1")
	pick := func(want string) {
		t.Helper()
		got, err := s.Pick(context.Background(), "claude", "model", opts, []*Auth{a, b})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v %v want %s", got, err, want)
		}
	}
	pick("a")
	b.Quota = resetTestAuth("b", now, 30*time.Minute).Quota
	pick("a")
	a.Disabled = true
	pick("b")
}
