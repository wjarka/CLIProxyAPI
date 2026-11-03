package management

import (
	"context"
	"net/http/httptest"
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
