package management

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaOverviewOmitsSecrets(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	_, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "account", Label: "Work", Provider: "codex", Metadata: map[string]any{"access_token": "do-not-expose-token", "websockets": true}, Attributes: map[string]string{"path": "/private/auth.json"}, Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"X-Codex-Plan-Type": "pro"}}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, "", m)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	h.GetQuotaOverview(ctx)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"websockets":true`) || !strings.Contains(body, `"label":"Work"`) {
		t.Fatalf("unexpected response: %s", body)
	}
	for _, secret := range []string{"do-not-expose-token", "/private/auth.json", "access_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response contains %s", secret)
		}
	}
}

func TestQuotaOverviewShowsReserveStatus(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{
		ReservePolicies: map[string]config.ReservePolicy{"work": {Timezone: "Europe/Warsaw", Schedule: []config.ReserveSchedule{{Days: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, ReleaseAt: "23:59", ProtectedBudgetPercent: 10}}}},
		AccountPolicies: []config.AccountReservePolicy{{Account: "work.json", ReservePolicy: "work"}},
	}}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	_, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "work.json", Provider: "claude", Quota: coreauth.QuotaState{
		ObservedAt: time.Now(), Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339), "Anthropic-Ratelimit-Unified-7d-Utilization": "0.95"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, "", m)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	h.GetQuotaOverview(ctx)
	body := rec.Body.String()
	for _, want := range []string{`"reserve":`, `"policy":"work"`, `"protected_percent":70`, `"remaining_percent":5`, `"reason":"quota_reserved"`} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %s: %s", want, body)
		}
	}
}

func TestQuotaOverviewShowsIndependentUsageGuard(t *testing.T) {
	protected := 30.0
	cfg := &config.Config{Routing: config.RoutingConfig{
		UsagePolicies:   map[string]config.UsagePolicy{"work-hours": {Timezone: "Europe/Warsaw", ProtectedBudgetPercent: &protected, Schedule: []config.UsageSchedule{{Days: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "09:00", End: "17:00"}}}},
		AccountPolicies: []config.AccountReservePolicy{{Account: "work.json", UsagePolicy: "work-hours"}},
	}}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	now := time.Now()
	_, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "work.json", Provider: "claude", Metadata: map[string]any{"access_token": "never-expose"}, Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.7", "Anthropic-Ratelimit-Unified-5h-Reset": now.Add(time.Hour).Format(time.RFC3339),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, "", m)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	h.GetQuotaOverview(ctx)
	var body struct {
		Accounts []struct {
			ID      string                       `json:"id"`
			Usage   *coreauth.QuotaUsageStatus   `json:"usage_policy"`
			Reserve *coreauth.QuotaReserveStatus `json:"reserve"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("unexpected accounts: %s", rec.Body.String())
	}
	status := body.Accounts[0].Usage
	if status == nil || status.Policy != "work-hours" || status.ProtectedPercent != 30 || status.RemainingPercent == nil || *status.RemainingPercent != 30 || status.UsedPercent == nil || *status.UsedPercent != 70 || len(status.Schedule) != 1 || status.Timezone != "Europe/Warsaw" || body.Accounts[0].Reserve != nil {
		t.Fatalf("unexpected policy status: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "never-expose") {
		t.Fatal("usage status leaked credentials")
	}
}

func TestQuotaUsagePolicyValidatedReloadPreservesWeeklyReserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `config-version: 8
routing:
  strategy: earliest-reset
  reserve-policies:
    work:
      timezone: Europe/Warsaw
      schedule:
        - days: [mon, tue, wed, thu, fri]
          protected-budget-percent: 15
          release-at: "17:00"
  account-policies:
    - account: work.json
      reserve-policy: work
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	reloads := make(chan *config.Config, 2)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloads <- cfg })
	router := gin.New()
	router.PATCH("/v8/management/config", h.ConfigV8)
	patch := `{"routing":{"usage-policies":{"work-hours":{"timezone":"Europe/Warsaw","protected-budget-percent":30,"schedule":[{"days":["mon","tue","wed","thu","fri"],"start":"09:00","end":"17:00"}]}},"account-policies":[{"account":"work.json","reserve-policy":"work","usage-policy":"work-hours"}]}}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v8/management/config", strings.NewReader(patch)))
	if rec.Code != 200 {
		t.Fatalf("patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reloaded *config.Config
	select {
	case reloaded = <-reloads:
	case <-time.After(5 * time.Second):
		t.Fatal("configuration reload did not complete")
	}
	if reloaded.Routing.Strategy != "earliest-reset" || len(reloaded.Routing.ReservePolicies["work"].Schedule) != 1 || reloaded.Routing.AccountPolicies[0].UsagePolicy != "work-hours" {
		t.Fatal("reload lost quota policies")
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routing.AccountPolicies[0].ReservePolicy != "work" || loaded.Routing.UsagePolicies["work-hours"].ProtectedBudgetPercent == nil || *loaded.Routing.UsagePolicies["work-hours"].ProtectedBudgetPercent != 30 {
		t.Fatal("persisted configuration lost policies")
	}
	saved, _ := os.ReadFile(path)
	invalid := strings.Replace(patch, `"protected-budget-percent":30`, `"protected-budget-percent":101`, 1)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v8/management/config", strings.NewReader(invalid)))
	after, _ := os.ReadFile(path)
	select {
	case <-reloads:
		t.Fatal("invalid patch triggered reload")
	default:
	}
	if rec.Code != 422 || string(saved) != string(after) {
		t.Fatalf("invalid patch changed config: %d %s", rec.Code, rec.Body.String())
	}
	unknown := strings.Replace(patch, `"protected-budget-percent":30`, `"protected-budget-percent":30,"protected-buget-percent":40`, 1)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v8/management/config", strings.NewReader(unknown)))
	after, _ = os.ReadFile(path)
	if rec.Code != 422 || string(saved) != string(after) {
		t.Fatalf("unknown policy field changed config: %d %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaUsagePolicyReplaceLegacyConsumedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `config-version: 8
routing:
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
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routing.UsagePolicies["work-hours"].ProtectedBudgetPercent; got == nil || *got != 30 {
		t.Fatal("startup did not preserve legacy policy protection")
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	body := `{"timezone":"Europe/Warsaw","protected-budget-percent":30,"schedule":[{"days":["mon","tue","wed","thu","fri"],"start":"09:00","end":"17:00"}]}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("PUT", "/v8/management/config/routing/usage-policies/work-hours", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("replace failed: %d %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "five-hour-used-percent") {
		t.Fatalf("unexpected persisted policy: %s", saved)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Routing.UsagePolicies["work-hours"].ProtectedBudgetPercent; got == nil || *got != 30 {
		t.Fatal("replacement did not preserve protected percentage")
	}
}
