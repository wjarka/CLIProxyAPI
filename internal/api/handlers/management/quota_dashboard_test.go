package management

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
