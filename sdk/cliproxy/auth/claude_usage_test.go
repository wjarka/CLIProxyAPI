package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeUsageSharedWithReserveAndStaleOnRateLimit(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(w, `{"seven_day":{"utilization":5,"resets_at":%q}}`, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, nil, nil)
	defer m.StopAutoRefresh()
	m.SetConfig(reserveTestConfig(t))
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	work, err := m.Register(WithSkipPersist(ctx), &Auth{ID: "work.json", Provider: "claude", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(WithSkipPersist(ctx), &Auth{ID: "personal.json", Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}

	m.RefreshReserveQuotas(ctx)
	fresh := m.ReadClaudeUsage(ctx, work.ID, ClaudeUsageQuota)
	if fresh.StatusCode != http.StatusOK || fresh.Stale || fresh.Body == "" || calls.Load() != 1 {
		t.Fatalf("dashboard did not reuse reserve usage: %+v, calls=%d", fresh, calls.Load())
	}
	ageClaudeUsage(t, m, work.ID)

	stale := m.ReadClaudeUsage(ctx, work.ID, ClaudeUsageQuota)
	if stale.StatusCode != http.StatusOK || !stale.Stale || stale.Body != fresh.Body || time.Until(stale.RetryAt) < time.Minute {
		t.Fatalf("429 should keep prior quota with meaningful backoff: %+v", stale)
	}
	m.RefreshReserveQuotas(ctx)
	if reserve := m.QuotaReserveStatus(work, time.Now()); reserve == nil || !reserve.Blocked || reserve.Reason != "quota_unknown" {
		t.Fatalf("stale usage must not allow protected routing: %+v", reserve)
	}
	if grant := m.ReadClaudeUsage(ctx, work.ID, ClaudeUsageGrantPreflight); grant.StatusCode != http.StatusTooManyRequests || calls.Load() != 2 {
		t.Fatalf("grant lookup bypassed account backoff: %+v, calls=%d", grant, calls.Load())
	}
	if personal := m.ReadClaudeUsage(ctx, "personal.json", ClaudeUsageQuota); personal.StatusCode != http.StatusOK || calls.Load() != 3 {
		t.Fatalf("work backoff affected personal usage: %+v, calls=%d", personal, calls.Load())
	}

	value, _ := m.claudeUsage.Load(work.ID)
	value.(*claudeUsageAccount).retryAt = time.Now().Add(-time.Second)
	recovered := m.ReadClaudeUsage(ctx, work.ID, ClaudeUsageQuota)
	if recovered.Stale || recovered.StatusCode != http.StatusOK || !recovered.RetryAt.IsZero() || calls.Load() != 4 {
		t.Fatalf("cache did not recover: %+v, calls=%d", recovered, calls.Load())
	}
	if reserve := m.QuotaReserveStatus(work, time.Now()); reserve == nil || reserve.Blocked {
		t.Fatalf("fresh dashboard usage did not restore routing: %+v", reserve)
	}
}

func ageClaudeUsage(t *testing.T, m *Manager, id string) {
	t.Helper()
	entry, ok := m.claudeUsage.Load(id)
	if !ok {
		t.Fatal("expected a cached usage response")
	}
	entry.(*claudeUsageAccount).usage.freshUntil = time.Time{}
	m.reserveQuotaObservations.Delete(id)
}

func TestClaudeGrantPreflightBypassesFreshSnapshotButHonorsBackoff(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cedar_ember") != "1" || r.URL.Query().Get("skip_spend") != "1" {
			t.Error("expected read-only grant status query")
		}
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{"cedar_ember":{"eligible":true}}`)
			return
		}
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, nil, nil)
	defer m.StopAutoRefresh()
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	const id = "work.json"
	if _, err := m.Register(WithSkipPersist(ctx), &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	m.ReadClaudeUsage(ctx, id, ClaudeUsageGrantStatus)
	if cached := m.ReadClaudeUsage(ctx, id, ClaudeUsageGrantStatus); cached.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("expected cached grant status, got %+v, calls=%d", cached, calls.Load())
	}
	start := time.Now()
	preflight := m.ReadClaudeUsage(ctx, id, ClaudeUsageGrantPreflight)
	if preflight.StatusCode != http.StatusTooManyRequests || preflight.RetryAt.Before(start.Add(10*time.Minute)) || calls.Load() != 2 {
		t.Fatalf("expected fresh preflight to respect upstream Retry-After: %+v, calls=%d", preflight, calls.Load())
	}
	if blocked := m.ReadClaudeUsage(ctx, id, ClaudeUsageGrantPreflight); blocked.StatusCode != http.StatusTooManyRequests || calls.Load() != 2 {
		t.Fatalf("preflight bypassed backoff: %+v, calls=%d", blocked, calls.Load())
	}
}
