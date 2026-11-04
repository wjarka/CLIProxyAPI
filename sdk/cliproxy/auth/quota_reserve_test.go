package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func reserveTestConfig(t *testing.T) *internalconfig.Config {
	t.Helper()
	cfg, err := internalconfig.ParseConfigBytes([]byte(`routing:
  strategy: earliest-reset
  reserve-policies:
    work:
      timezone: Europe/Warsaw
      schedule:
        - days: [mon, wed, thu, fri]
          protected-budget-percent: 10
          release-at: "17:00"
        - days: [tue]
          protected-budget-percent: 20
          release-at: "17:00"
  account-policies:
    - account: work.json
      reserve-policy: work
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func reserveTestTime(t *testing.T, raw string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func reserveTestAuth(now, reset time.Time, used string) *Auth {
	a := resetTestAuth("work.json", now, reset.Sub(now))
	a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization")] = used
	return a
}

// Removing the manager's reserve filter would select the protected credential.
func TestReserveWednesdayResetAndReleaseBoundaries(t *testing.T) {
	reset := reserveTestTime(t, "2026-11-11T05:00:00+01:00")
	for _, tc := range []struct {
		at, used string
		blocked  bool
	}{
		{"2026-11-04T09:00:00+01:00", "0.40", true},  // 60 points reserved
		{"2026-11-04T17:00:00+01:00", "0.40", false}, // 50 points reserved
		{"2026-11-05T17:00:00+01:00", "0.60", true},  // 40 points reserved
		{"2026-11-06T17:00:00+01:00", "0.70", true},  // 30 points reserved
		{"2026-11-09T17:00:00+01:00", "0.80", true},  // 20 points reserved
		{"2026-11-10T09:00:00+01:00", "0.799", false},
		{"2026-11-10T09:00:00+01:00", "0.80", true},
		{"2026-11-10T13:00:00+01:00", "0.88", true}, // Desktop can spend below floor
		{"2026-11-10T16:59:59+01:00", "0.88", true},
		{"2026-11-10T17:00:00+01:00", "0.88", false},
	} {
		t.Run(tc.at+"/"+tc.used, func(t *testing.T) {
			now := reserveTestTime(t, tc.at)
			m := NewManager(nil, &EarliestResetSelector{}, nil)
			m.SetConfig(reserveTestConfig(t))
			_, err := m.availableAuthsForRouteModel([]*Auth{reserveTestAuth(now, reset, tc.used)}, "claude", "work/claude-sonnet", now)
			if (err != nil) != tc.blocked {
				t.Fatalf("blocked = %v (%v), want %v", err != nil, err, tc.blocked)
			}
			if tc.blocked && !strings.Contains(err.Error(), "quota_reserved") {
				t.Fatalf("missing reserve error: %v", err)
			}
		})
	}
}

func TestReserveRejectsUnknownQuotaWithoutAffectingOtherAccounts(t *testing.T) {
	now := reserveTestTime(t, "2026-11-10T09:00:00+01:00")
	reset := now.Add(20 * time.Hour)
	for _, change := range []func(*Auth){
		func(a *Auth) { a.Quota = QuotaState{} },
		func(a *Auth) { a.Quota.ObservedAt = now.Add(-31 * time.Minute) },
		func(a *Auth) {
			a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization")] = "NaN"
		},
		func(a *Auth) {
			a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization")] = "1.1"
		},
		func(a *Auth) {
			delete(a.Quota.Signals, http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization"))
		},
		func(a *Auth) { a.Quota.ObservedAt = now.Add(time.Minute) },
		func(a *Auth) {
			a.Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-reset")] = strconv.FormatInt(now.Unix(), 10)
		},
	} {
		m := NewManager(nil, nil, nil)
		m.SetConfig(reserveTestConfig(t))
		a := reserveTestAuth(now, reset, "0.1")
		change(a)
		available, err := m.availableAuthsForRouteModel([]*Auth{a, {ID: "personal", Provider: "claude"}}, "claude", "model", now)
		if err != nil || len(available) != 1 || available[0].ID != "personal" {
			t.Fatalf("available = %v, err=%v", available, err)
		}
	}
}

func TestReserveStaleGraceWithinKnownWeeklyWindow(t *testing.T) {
	now := reserveTestTime(t, "2026-11-10T09:00:00+01:00")
	m := NewManager(nil, nil, nil)
	defer m.StopAutoRefresh()
	m.SetConfig(reserveTestConfig(t))
	for _, tc := range []struct {
		name, used, reason string
		age, untilReset    time.Duration
		stale, blocked     bool
	}{
		{"temporary outage above reserve", "0.15", "", 2 * time.Minute, 20 * time.Hour, true, false},
		{"last known balance at reserve", "0.80", "quota_reserved", 2 * time.Minute, 20 * time.Hour, true, true},
		{"grace expired", "0.15", "quota_unknown", 31 * time.Minute, time.Hour, false, true},
		{"weekly reset passed", "0.15", "quota_unknown", 2 * time.Minute, -time.Second, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := reserveTestAuth(now.Add(-tc.age), now.Add(tc.untilReset), tc.used)
			status := m.QuotaReserveStatus(auth, now)
			if status == nil || status.Blocked != tc.blocked || status.Reason != tc.reason || status.Stale != tc.stale {
				t.Fatalf("reserve status = %+v, want blocked=%t reason=%q stale=%t", status, tc.blocked, tc.reason, tc.stale)
			}
			if tc.stale && status.RemainingPercent == nil {
				t.Fatalf("reserve dropped the valid stale balance: %+v", status)
			}
		})
	}
}

func TestReserveDSTUsesLocalDeadline(t *testing.T) {
	cfg := reserveTestConfig(t)
	p := cfg.Routing.ReservePolicies["work"]
	p.Schedule = []internalconfig.ReserveSchedule{{Days: []string{"sun"}, ProtectedBudgetPercent: 20, ReleaseAt: "17:00"}}
	for _, tc := range []struct {
		now, reset string
		want       float64
	}{
		{"2026-10-25T15:59:59Z", "2026-10-28T04:00:00Z", 20},
		{"2026-10-25T16:00:00Z", "2026-10-28T04:00:00Z", 0},
		{"2026-03-29T14:59:59Z", "2026-04-01T03:00:00Z", 20},
		{"2026-03-29T15:00:00Z", "2026-04-01T03:00:00Z", 0},
	} {
		got, _ := p.ProtectedReserve(reserveTestTime(t, tc.now), reserveTestTime(t, tc.reset))
		if got != tc.want {
			t.Fatalf("at %s reserve=%v want=%v", tc.now, got, tc.want)
		}
	}
}

// Real manager paths must filter before affinity, priority and explicit pinning.
func TestReserveManagerSelectionCannotBypassProtection(t *testing.T) {
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
			cfg := reserveTestConfig(t)
			cfg.Routing.ReservePolicies["work"] = internalconfig.ReservePolicy{Timezone: "Europe/Warsaw", Schedule: []internalconfig.ReserveSchedule{{Days: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, ReleaseAt: "23:59", ProtectedBudgetPercent: 10}}}
			m.SetConfig(cfg)
			m.RegisterExecutor(&quotaAttemptIsolationExecutor{})
			now := time.Now()
			a := reserveTestAuth(now, now.Add(7*24*time.Hour), "0")
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
			m.auths["work.json"].Quota.ObservedAt = now.Add(-2 * time.Minute)
			m.mu.Unlock()
			selected, err = m.SelectAuth(context.Background(), "claude", model, opts)
			if err != nil || selected.ID != "work.json" {
				t.Fatalf("stale above-reserve account lost selection: %v", err)
			}
			m.mu.Lock()
			m.auths["work.json"].Quota.Signals[http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization")] = "0.90"
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

type reserveClaudeExecutor struct{ quotaAttemptIsolationExecutor }

func (*reserveClaudeExecutor) Identifier() string { return "claude" }

func TestReserveExcludesDeadlineEqualToReset(t *testing.T) {
	p := reserveTestConfig(t).Routing.ReservePolicies["work"]
	now := reserveTestTime(t, "2026-11-10T09:00:00+01:00")
	reset := reserveTestTime(t, "2026-11-10T17:00:00+01:00")
	got, _ := p.ProtectedReserve(now, reset)
	if got != 0 {
		t.Fatalf("reserve at equal reset deadline=%v want0", got)
	}
}

func TestReserveRejectsHomeDispatch(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := reserveTestConfig(t)
	cfg.Home.Enabled = true
	m.SetConfig(cfg)
	_, err := m.pickHomeDispatchSelection(context.Background(), "model", cliproxyexecutor.Options{})
	if err == nil || !strings.Contains(err.Error(), "quota_reserve_unsupported") {
		t.Fatalf("Home did not reject unenforceable reserves: %v", err)
	}
}

func TestReserveFailsClosedAfterGraceEvenAfterFinalRelease(t *testing.T) {
	now := reserveTestTime(t, "2026-11-10T18:00:00+01:00")
	a := reserveTestAuth(now.Add(-31*time.Minute), reserveTestTime(t, "2026-11-11T05:00:00+01:00"), "0.8")
	m := NewManager(nil, nil, nil)
	m.SetConfig(reserveTestConfig(t))
	if status := m.QuotaReserveStatus(a, now); status == nil || !status.Blocked {
		t.Fatalf("stale quota admitted after release: %+v", status)
	}
}

func TestReserveMixedRetriesAndPluginDelegation(t *testing.T) {
	for _, delegate := range []string{"", pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst} {
		t.Run(delegate, func(t *testing.T) {
			m := NewManager(nil, &EarliestResetSelector{}, nil)
			cfg := reserveTestConfig(t)
			cfg.Routing.ReservePolicies["work"] = internalconfig.ReservePolicy{Timezone: "Europe/Warsaw", Schedule: []internalconfig.ReserveSchedule{{Days: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, ReleaseAt: "23:59", ProtectedBudgetPercent: 10}}}
			m.SetConfig(cfg)
			m.RegisterExecutor(&reserveClaudeExecutor{})
			if delegate != "" {
				m.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: delegate}})
			}
			now := time.Now()
			work := reserveTestAuth(now, now.Add(7*24*time.Hour), "0.95")
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
