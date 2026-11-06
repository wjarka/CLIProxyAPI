package auth

import (
	"context"
	"fmt"

	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const usageTestYAML = `routing:
  usage-policies:
    work-hours:
      timezone: Europe/Warsaw
      five-hour-used-percent: 70
      schedule:
        - days: [mon, tue, wed, thu, fri]
          start: "09:00"
          end: "17:00"
  account-policies:
    - account: work.json
      usage-policy: work-hours
`

func usageTestConfig(t *testing.T) *internalconfig.Config {
	t.Helper()
	cfg, err := internalconfig.ParseConfigBytes([]byte(usageTestYAML))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func usageTestAuth(now time.Time, used string) *Auth {
	a := reserveTestAuth(now, now.Add(6*24*time.Hour), "0.01")
	a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-utilization")] = used
	a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-reset")] = now.Add(time.Hour).Format(time.RFC3339)
	return a
}
func TestUsageGuardSelectionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		at, used string
		blocked  bool
	}{
		{"2026-11-06T08:59:59+01:00", "0.9", false},
		{"2026-11-06T09:00:00+01:00", "0.7", true},
		{"2026-11-06T12:00:00+01:00", "0.699", false},
		{"2026-11-06T12:00:00+01:00", "0.701", true},
		{"2026-11-06T16:59:59+01:00", "0.7", true},
		{"2026-11-06T17:00:00+01:00", "0.9", false},
		{"2026-11-07T12:00:00+01:00", "0.9", false},
	} {
		t.Run(tc.at+tc.used, func(t *testing.T) {
			now := reserveTestTime(t, tc.at)
			m := NewManager(nil, nil, nil)
			m.SetConfig(usageTestConfig(t))
			_, err := m.availableAuthsForRouteModel([]*Auth{usageTestAuth(now, tc.used)}, "claude", "work/claude-sonnet", now)
			if (err != nil) != tc.blocked {
				t.Fatalf("blocked=%v err=%v want%v", err != nil, err, tc.blocked)
			}
			if err != nil && !strings.Contains(err.Error(), "quota_usage_guard") {
				t.Fatalf("wrong reason: %v", err)
			}
		})
	}
}
func TestUsageGuardUnknownQuotaAndUnprotectedAccounts(t *testing.T) {
	now := reserveTestTime(t, "2026-11-06T12:00:00+01:00")
	for name, mutate := range map[string]func(*Auth){
		"stale":  func(a *Auth) { a.Quota.ObservedAt = now.Add(-2 * time.Minute) },
		"future": func(a *Auth) { a.Quota.ObservedAt = now.Add(time.Minute) },
		"missing": func(a *Auth) {
			delete(a.Quota.Signals, http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-utilization"))
		},
		"NaN": func(a *Auth) {
			a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-utilization")] = "NaN"
		},
		"overflow": func(a *Auth) {
			a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-utilization")] = "1.1"
		},
		"expired": func(a *Auth) {
			a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-reset")] = now.Format(time.RFC3339)
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := usageTestAuth(now, "0.1")
			mutate(a)
			m := NewManager(nil, nil, nil)
			m.SetConfig(usageTestConfig(t))
			got, err := m.availableAuthsForRouteModel([]*Auth{a, {ID: "personal", Provider: "claude"}}, "claude", "model", now)
			if err != nil || len(got) != 1 || got[0].ID != "personal" {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
}

func usageAlwaysTestConfig(t *testing.T) *internalconfig.Config {
	t.Helper()
	cfg := usageTestConfig(t)
	p := cfg.Routing.UsagePolicies["work-hours"]
	p.Schedule = []internalconfig.UsageSchedule{{Days: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, Start: "00:00", End: "23:59"}}
	// Keep a real manager test independent of the current time of day.
	p.Timezone = "UTC"
	if p.ActiveUntil(time.Now()).IsZero() {
		p.Timezone = "Europe/Warsaw"
	}
	cfg.Routing.UsagePolicies["work-hours"] = p
	return cfg
}
func TestUsageGuardCombinesWithWeeklyReserve(t *testing.T) {
	now := reserveTestTime(t, "2026-11-06T12:00:00+01:00")
	cfg := usageTestConfig(t)
	cfg.Routing.ReservePolicies = reserveTestConfig(t).Routing.ReservePolicies
	cfg.Routing.AccountPolicies[0].ReservePolicy = "work"
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	for _, tc := range []struct{ weekly, short, reason string }{
		{"0.01", "0.1", ""}, {"0.95", "0.1", "quota_reserved"}, {"0.01", "0.7", "quota_usage_guard"},
	} {
		a := usageTestAuth(now, tc.short)
		a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization")] = tc.weekly
		_, err := m.availableAuthsForRouteModel([]*Auth{a}, "claude", "model", now)
		if tc.reason == "" && err != nil {
			t.Fatal(err)
		}
		if tc.reason != "" && (err == nil || !strings.Contains(err.Error(), tc.reason)) {
			t.Fatalf("reason=%s err=%v", tc.reason, err)
		}
	}
	outside := reserveTestTime(t, "2026-11-06T18:00:00+01:00")
	a := usageTestAuth(outside, "0.9")
	a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization")] = "0.95"
	if reason := m.quotaPolicyBlockReason(a, outside); reason != "quota_reserved" {
		t.Fatalf("weekly protection lost outside hours: %s", reason)
	}
}
func TestUsageGuardCodexSelectsFiveHourWindowByDuration(t *testing.T) {
	now := reserveTestTime(t, "2026-11-06T12:00:00+01:00")
	for _, window := range []string{"primary", "secondary"} {
		a := &Auth{ID: "work.json", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{}}}
		for key, value := range map[string]string{"window-minutes": "300", "used-percent": "70", "reset-after-seconds": "3600"} {
			a.Quota.Signals[http.CanonicalHeaderKey("x-codex-"+window+"-"+key)] = value
		}
		m := NewManager(nil, nil, nil)
		m.SetConfig(usageTestConfig(t))
		status := m.QuotaUsageStatus(a, now)
		if status == nil || !status.Blocked || status.UsedPercent == nil || *status.UsedPercent != 70 || !status.ResetAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("wrong Codex status: %+v", status)
		}
	}
}
func TestUsageGuardInactiveDoesNotRequireQuota(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(usageTestConfig(t))
	a := &Auth{ID: "work.json", Provider: "claude"}
	now := reserveTestTime(t, "2026-11-06T18:00:00+01:00")
	if status := m.QuotaUsageStatus(a, now); status == nil || status.Active || status.Blocked {
		t.Fatalf("inactive guard blocks missing quota: %+v", status)
	}
}
func TestUsageGuardRefreshesMissingFiveHourDespiteFreshWeekly(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		weekly     bool
		valid      bool
	}{
		{"usage only", `"five_hour":{"utilization":10,"resets_at":%q}`, false, true},
		{"both", `"five_hour":{"utilization":70,"resets_at":%q},"seven_day":{"utilization":1,"resets_at":%q}`, true, true},
		{"weekly only", `"seven_day":{"utilization":1,"resets_at":%q}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			now := time.Now()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/oauth/usage" {
					t.Errorf("unexpected usage read: %s %s", r.Method, r.URL.Path)
				}
				args := []any{now.Add(time.Hour).Format(time.RFC3339)}
				if tc.name == "both" {
					args = append(args, now.Add(6*24*time.Hour).Format(time.RFC3339))
				}
				fmt.Fprint(w, "{"+fmt.Sprintf(tc.body, args...)+"}")
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			cfg := usageAlwaysTestConfig(t)
			if tc.weekly {
				cfg.Routing.ReservePolicies = reserveTestConfig(t).Routing.ReservePolicies
				cfg.Routing.AccountPolicies[0].ReservePolicy = "work"
			}
			m := NewManager(nil, nil, nil)
			m.SetConfig(cfg)
			m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
			a := reserveTestAuth(now, now.Add(6*24*time.Hour), "0.01")
			if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
				t.Fatal(err)
			}
			m.RefreshReserveQuotas(context.Background())
			got := m.reserveQuotaAuth(m.List()[0])
			_, used, _ := fiveHourUsageWindow(got, time.Now())
			if calls.Load() != 1 || (used != nil) != tc.valid {
				t.Fatalf("calls=%d used=%v valid=%v", calls.Load(), used, tc.valid)
			}
			if tc.valid {
				m.RefreshReserveQuotas(context.Background())
				if calls.Load() != 1 {
					t.Fatal("fresh usage triggered another read")
				}
			}
			status := m.QuotaUsageStatus(got, time.Now())
			if tc.name == "both" && (!status.Blocked || status.Reason != "five_hour_limit") {
				t.Fatalf("external consumption not enforced: %+v", status)
			}
			if !tc.valid && (!status.Blocked || status.Reason != "quota_unknown") {
				t.Fatalf("partial response allowed usage: %+v", status)
			}
		})
	}
}

func TestUsageGuardManagerSelectionCannotBypassProtection(t *testing.T) {
	for _, strategy := range []string{"round-robin", "earliest-reset", "affinity"} {
		t.Run(strategy, func(t *testing.T) {
			var selector Selector = &RoundRobinSelector{}
			if strategy == "earliest-reset" {
				selector = &EarliestResetSelector{}
			}
			if strategy == "affinity" {
				s := NewSessionAffinitySelector(&EarliestResetSelector{})
				defer s.Stop()
				selector = s
			}
			m := NewManager(nil, selector, nil)
			cfg := usageAlwaysTestConfig(t)
			m.SetConfig(cfg)
			m.RegisterExecutor(&quotaAttemptIsolationExecutor{})
			now := time.Now()
			a := usageTestAuth(now, "0")
			a.Attributes = map[string]string{"priority": "10"}
			const model = "reserve-selection-model"
			for _, candidate := range []*Auth{a, {ID: "personal", Provider: "codex"}} {
				if candidate.ID == "personal" {
					candidate.Provider = "claude"
				}
				registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
				if _, err := m.Register(WithSkipPersist(context.Background()), candidate); err != nil {
					t.Fatal(err)
				}
			}
			m.RegisterExecutor(&reserveClaudeExecutor{})
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"protected-thread"}}}
			selected, err := m.SelectAuth(context.Background(), "claude", model, opts)
			if err != nil || selected.ID != "work.json" {
				t.Fatalf("initial pick=%v %v", selected, err)
			}
			m.mu.Lock()
			m.auths["work.json"].Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-5h-utilization")] = "0.90"
			m.mu.Unlock()
			selected, err = m.SelectAuth(context.Background(), "claude", model, opts)
			if err != nil || selected.ID != "personal" {
				t.Fatalf("protected pick=%v %v", selected, err)
			}
			opts.Metadata = map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "work.json"}
			if selected, err = m.SelectAuth(context.Background(), "claude", model, opts); err == nil {
				t.Fatalf("pinned credential bypassed reserve: %v", selected)
			}
		})
	}
}

func TestUsageGuardMixedRetriesAndPluginDelegation(t *testing.T) {
	for _, delegate := range []string{"", pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst} {
		t.Run(delegate, func(t *testing.T) {
			m := NewManager(nil, &EarliestResetSelector{}, nil)
			cfg := usageAlwaysTestConfig(t)
			m.SetConfig(cfg)
			m.RegisterExecutor(&reserveClaudeExecutor{})
			if delegate != "" {
				m.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: delegate}})
			}
			now := time.Now()
			work := usageTestAuth(now, "0.95")
			work.Attributes = map[string]string{"priority": "10"}
			for _, a := range []*Auth{work, {ID: "personal-a", Provider: "claude"}, {ID: "personal-b", Provider: "claude"}} {
				if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
					t.Fatal(err)
				}
			}
			tried := map[string]struct{}{"personal-a": {}}
			selected, _, _, err := m.pickNextMixed(context.Background(), []string{"claude"}, "", cliproxyexecutor.Options{}, tried)
			if err != nil || selected.ID != "personal-b" {
				t.Fatalf("mixed retry selected %v, %v", selected, err)
			}
			m.SetConfig(&internalconfig.Config{})
			selected, _, err = m.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
			if err != nil || selected.ID != "work.json" {
				t.Fatalf("removing policy did not restore account: %v %v", selected, err)
			}
		})
	}
}

func TestUsageGuardConfiguredThresholdEquality(t *testing.T) {
	now := reserveTestTime(t, "2026-11-06T12:00:00+01:00")
	cfg := usageTestConfig(t)
	p := cfg.Routing.UsagePolicies["work-hours"]
	limit := 58.0
	p.FiveHourUsedPercent = &limit
	cfg.Routing.UsagePolicies["work-hours"] = p
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	for _, tc := range []struct {
		used    string
		blocked bool
	}{{"0.58", true}, {"0.57999", false}} {
		status := m.QuotaUsageStatus(usageTestAuth(now, tc.used), now)
		if status.Blocked != tc.blocked {
			t.Fatalf("used=%s blocked=%v want%v", tc.used, status.Blocked, tc.blocked)
		}
	}
}
