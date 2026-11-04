package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Only the cloud HTTP transport is replaced; manager selection and observation
// updates run unchanged against a real local HTTP server.
type reserveUsageExecutor struct {
	reserveClaudeExecutor
	endpoint *url.URL
}

func TestReserveUsageNormalization(t *testing.T) {
	now := reserveTestTime(t, "2026-11-10T09:00:00+01:00")
	for _, tc := range []struct {
		provider, body string
		remaining      float64
		valid          bool
	}{
		{"claude", `{"seven_day":{"utilization":80,"resets_at":"2026-11-11T04:00:00Z"}}`, 20, true},
		{"claude", `{"seven_day":{"utilization":0,"resets_at":"2026-11-11T04:00:00Z"}}`, 100, true},
		{"claude", `{"seven_day":{"utilization":80,"resets_at":"2026-11-11T04:00:00Z"},"limits":[],"member_dashboard_available":true,"weekly_scoped_shares":[],"spend":{"used":{},"percent":100}}`, 20, true},
		{"claude", `{"seven_day":{"utilization":101,"resets_at":"2026-11-11T04:00:00Z"}}`, 0, false},
		{"claude", `{"seven_day":null}`, 0, false},
		{"claude", `{"seven_day":{"utilization":80,"resets_at":"2026-11-09T04:00:00Z"}}`, 0, false},
		{"codex", `{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_at":1794369600}}}`, 20, true},
		{"codex", `{"rate_limit":{"secondary_window":{"used_percent":95,"limit_window_seconds":604800,"reset_at":1794369600}}}`, 5, true},
		{"codex", `{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":1794369600}}}`, 0, false},
	} {
		headers, err := reserveUsageHeaders(tc.provider, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		quota := QuotaState{}
		quota.ObserveResponseHeadersForProvider(tc.provider, headers, now)
		_, remaining, _ := weeklyReserveWindow(&Auth{Provider: tc.provider, Quota: quota}, now)
		if (remaining != nil) != tc.valid {
			t.Fatalf("%s valid=%v want%v", tc.body, remaining != nil, tc.valid)
		}
		if remaining != nil && math.Abs(*remaining-tc.remaining) > 1e-9 {
			t.Fatalf("remaining=%v want%v", *remaining, tc.remaining)
		}
	}
}

func (e *reserveUsageExecutor) HttpRequest(ctx context.Context, _ *Auth, req *http.Request) (*http.Response, error) {
	copy := req.Clone(ctx)
	copy.URL.Scheme = e.endpoint.Scheme
	copy.URL.Host = e.endpoint.Host
	return http.DefaultClient.Do(copy)
}

func TestReserveColdStartAndExternalUsageRefresh(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/oauth/usage" {
					t.Errorf("unexpected usage request: %s %s", r.Method, r.URL.Path)
				}
				if fail {
					w.WriteHeader(503)
					return
				}
				used := 5
				if calls.Load() > 1 {
					used = 95
				}
				fmt.Fprintf(w, `{"seven_day":{"utilization":%d,"resets_at":%q}}`, used, time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339))
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			m := NewManager(nil, &EarliestResetSelector{}, nil)
			cfg := reserveTestConfig(t)
			cfg.Routing.ReservePolicies["work"] = internalconfig.ReservePolicy{Timezone: "Europe/Warsaw", Schedule: []internalconfig.ReserveSchedule{{Days: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, ReleaseAt: "23:59", ProtectedBudgetPercent: 10}}}
			m.SetConfig(cfg)
			m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
			const model = "reserve-refresh-model"
			registry.GetGlobalRegistry().RegisterClient("work.json", "claude", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient("work.json")
			if _, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "work.json", Provider: "claude", Status: StatusActive}); err != nil {
				t.Fatal(err)
			}
			selected, err := m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{})
			if fail {
				if err == nil {
					t.Fatal("usage refresh failure bypassed protection")
				}
				_, _ = m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{})
				if calls.Load() != 1 {
					t.Fatalf("failed refreshes not rate limited: %d", calls.Load())
				}
				return
			}
			if err != nil || selected.ID != "work.json" {
				t.Fatalf("cold start pick=%v %v", selected, err)
			}
			_, err = m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{})
			if err != nil || calls.Load() != 1 {
				t.Fatalf("fresh observation not reused: calls=%d err=%v", calls.Load(), err)
			}
			m.mu.Lock()
			m.auths["work.json"].Quota.ObservedAt = time.Now().Add(-2 * time.Minute)
			m.mu.Unlock()
			ageClaudeUsage(t, m, "work.json")
			// External consumption is now visible; no inference request is needed.
			if _, err = m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{}); err == nil {
				t.Fatal("external usage did not protect account")
			}
			if calls.Load() != 2 {
				t.Fatalf("usage reads=%d want2", calls.Load())
			}
		})
	}
}

func TestReserveConcurrentColdStartSharesUsageRead(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"seven_day":{"utilization":0,"resets_at":%q}}`, time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, &EarliestResetSelector{}, nil)
	m.SetConfig(reserveTestConfig(t))
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	const model = "reserve-concurrent-model"
	registry.GetGlobalRegistry().RegisterClient("work.json", "claude", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient("work.json")
	_, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "work.json", Provider: "claude", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.SelectAuth(context.Background(), "claude", model, cliproxyexecutor.Options{}); err != nil {
				t.Errorf("pick: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("usage reads=%d want1", calls.Load())
	}
}

func TestReserveRefreshPreservesPassiveObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"seven_day":{"utilization":95,"resets_at":%q}}`, time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, nil, nil)
	defer m.StopAutoRefresh()
	m.SetConfig(reserveTestConfig(t))
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	observed := time.Now().Add(-2 * time.Minute)
	a := reserveTestAuth(observed, time.Now().Add(7*24*time.Hour), "0.5")
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Utilization"] = "0.3"
	a.Quota.Signals["Anthropic-Ratelimit-Unified-Fallback"] = "available"
	_, err := m.Register(WithSkipPersist(context.Background()), a)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.refreshOneReserveQuota(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	after := m.List()[0]
	if !after.Quota.ObservedAt.Equal(observed) || after.Quota.Signals["Anthropic-Ratelimit-Unified-Fallback"] != "available" || after.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Utilization"] != "0.3" {
		t.Fatalf("usage read replaced passive observation: %+v", after.Quota)
	}
	status := m.QuotaReserveStatus(after, time.Now())
	if status.RemainingPercent == nil || math.Abs(*status.RemainingPercent-5) > 1e-9 {
		t.Fatalf("fresh reserve balance missing: %+v", status)
	}
}

func TestReserveStalledUsageDoesNotHoldPersonalSelection(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(release); server.Close() }()
	endpoint, _ := url.Parse(server.URL)
	m := NewManager(nil, &EarliestResetSelector{}, nil)
	defer m.StopAutoRefresh()
	m.SetConfig(reserveTestConfig(t))
	m.RegisterExecutor(&reserveUsageExecutor{endpoint: endpoint})
	for _, a := range []*Auth{{ID: "work.json", Provider: "claude"}, {ID: "personal", Provider: "claude"}} {
		if _, err := m.Register(WithSkipPersist(context.Background()), a); err != nil {
			t.Fatal(err)
		}
	}
	result := make(chan error, 1)
	go func() {
		a, err := m.SelectAuth(context.Background(), "claude", "", cliproxyexecutor.Options{})
		if err == nil && a.ID != "personal" {
			err = fmt.Errorf("selected %s", a.ID)
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("usage refresh never started")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled quota read held healthy personal account")
	}
}
