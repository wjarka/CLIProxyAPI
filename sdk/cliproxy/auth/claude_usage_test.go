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
	if reserve := m.QuotaReserveStatus(work, time.Now()); reserve == nil || reserve.Blocked || !reserve.Stale {
		t.Fatalf("last known above-reserve balance should keep routing through backoff: %+v", reserve)
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
	if value, ok := m.reserveQuotaObservations.Load(id); ok {
		observation := value.(reserveQuotaObservation)
		observation.quota.ObservedAt = time.Now().Add(-2 * time.Minute)
		m.reserveQuotaObservations.Store(id, observation)
	}
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

func TestClaudeUsageGuardSharesCacheAndExpiresAtFiveHourReset(t *testing.T) {
	for _, weekly := range []bool{false, true} {
		t.Run(fmt.Sprint(weekly), func(t *testing.T) {
			var calls atomic.Int32
			reset := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprintf(w, `{"five_hour":{"utilization":10,"resets_at":%q}`, reset.Format(time.RFC3339))
				if weekly {
					fmt.Fprintf(w, `,"seven_day":{"utilization":1,"resets_at":%q}`, reset.Add(5*24*time.Hour).Format(time.RFC3339))
				}
				fmt.Fprint(w, "}")
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			m := NewManager(nil, nil, nil)
			defer m.StopAutoRefresh()
			m.SetConfig(usageAlwaysTestConfig(t))
			m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
			work, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "work.json", Provider: "claude", Status: StatusActive})
			if err != nil {
				t.Fatal(err)
			}
			first := m.ReadClaudeUsage(context.Background(), work.ID, ClaudeUsageQuota)
			if first.StatusCode != 200 || !first.freshUntil.Equal(reset) {
				t.Fatalf("cache expiry=%v want %v", first.freshUntil, reset)
			}
			status := m.QuotaUsageStatus(work, time.Now())
			if status.UsedPercent == nil || *status.UsedPercent != 10 || status.Blocked {
				t.Fatalf("valid five-hour usage not available: %+v", status)
			}
			m.ReadClaudeUsage(context.Background(), work.ID, ClaudeUsageQuota)
			if calls.Load() != 1 {
				t.Fatalf("guard reader did not share cached usage: %d calls", calls.Load())
			}
		})
	}
}

func TestClaudeUsageExpiredFiveHourDoesNotDisableWeeklyCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"five_hour":{"utilization":70,"resets_at":%q},"seven_day":{"utilization":5,"resets_at":%q}}`, time.Now().Add(-time.Minute).Format(time.RFC3339), time.Now().Add(24*time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, nil, nil)
	defer m.StopAutoRefresh()
	m.SetConfig(reserveTestConfig(t))
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	work, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "work.json", Provider: "claude", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	m.ReadClaudeUsage(context.Background(), work.ID, ClaudeUsageQuota)
	m.ReadClaudeUsage(context.Background(), work.ID, ClaudeUsageQuota)
	if calls.Load() != 1 {
		t.Fatalf("expired short window disabled cache: %d reads", calls.Load())
	}
}

func TestClaudeUsagePartialFiveHourPreservesWeeklyGrace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"utilization":10,"resets_at":%q}}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, nil, nil)
	defer m.StopAutoRefresh()
	m.SetConfig(reserveTestConfig(t))
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	now := time.Now()
	work, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "work.json", Provider: "claude", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	old := reserveTestAuth(now.Add(-2*time.Minute), now.Add(6*24*time.Hour), "0.01")
	m.reserveQuotaObservations.Store(work.ID, reserveQuotaObservation{epoch: work.RegistrationEpoch, quota: old.Quota})
	m.ReadClaudeUsage(context.Background(), work.ID, ClaudeUsageQuota)
	status := m.QuotaReserveStatus(work, time.Now())
	if status == nil || status.Blocked || !status.Stale || !status.ObservedAt.Equal(old.Quota.ObservedAt) {
		t.Fatalf("partial response discarded or redated weekly snapshot: %+v", status)
	}
}
