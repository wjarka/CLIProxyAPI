package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

func TestHistoryReadRangeAndFailureIndependent(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	p := 23.0
	store, _ := quotahistory.NewStore(filepath.Join(t.TempDir(), "h.json"), quotahistory.Retention)
	_ = store.SetAccounts([]quotahistory.Account{{ID: "a", Provider: "claude", Label: "A", CredentialIDs: []string{"index"}}})
	_ = store.Observe(quotahistory.PoolObservation{AccountID: "a", Provider: "claude", PoolID: "weekly", Label: "Weekly", ObservedAt: at, ResetAt: at.Add(time.Hour), RemainingPercent: &p})
	h := NewHandler(nil, "", nil)
	h.historyStore = store
	h.history = quotahistory.NewCollector(store, newQuotaHistorySource(h), time.Now)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?from="+at.Add(-time.Hour).Format(time.RFC3339)+"&to="+at.Add(2*time.Hour).Format(time.RFC3339), nil)
	h.GetQuotaHistory(ctx)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"remaining_percent":23`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", nil)
	h.GetQuotaHistory(ctx)
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
}
func TestHistoryPathNoConfigAndAuthDirProtection(t *testing.T) {
	if _, err := historyStoragePath(&config.Config{}, ""); err == nil {
		t.Fatal("wrote relative to cwd")
	}
	root := t.TempDir()
	cfg := &config.Config{AuthDir: filepath.Join(root, "auths")}
	path, err := historyStoragePath(cfg, filepath.Join(root, "config.yaml"))
	if err != nil || path != filepath.Join(root, "quota-history.json") {
		t.Fatal(path, err)
	}
	cfg.QuotaHistory.Path = "auths/history.json"
	if _, err = historyStoragePath(cfg, filepath.Join(root, "config.yaml")); err == nil {
		t.Fatal("auth watcher path accepted")
	}
}
func TestHistoryUnavailableAndInvalidRefresh(t *testing.T) {
	h := NewHandler(nil, "", nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	h.GetQuotaHistory(ctx)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["storage_status"] != "unavailable" {
		t.Fatal(body)
	}
	h.StartQuotaHistory(context.Background())
	h.StopQuotaHistory()
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"credential_ids":42}`))
	h.RefreshQuotaHistory(ctx)
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
}

func TestHistoryLifecycleRestartPreservesFinalBalance(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "quota-history.json")
	at := time.Now().UTC().Add(-2 * time.Hour)
	p := 23.0
	store, err := quotahistory.NewStore(path, quotahistory.Retention)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetAccounts([]quotahistory.Account{{ID: "fixture", Provider: "claude", Label: "Fixture", CredentialIDs: []string{"fixture-index"}}}); err != nil {
		t.Fatal(err)
	}
	if err = store.Observe(quotahistory.PoolObservation{AccountID: "fixture", Provider: "claude", PoolID: "weekly", Label: "Weekly", ObservedAt: at, ResetAt: at.Add(time.Minute), RemainingPercent: &p}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		h := NewHandler(&config.Config{AuthDir: filepath.Join(root, "auths")}, filepath.Join(root, "config.yaml"), nil)
		h.StartQuotaHistory(context.Background())
		h.StopQuotaHistory()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest("GET", "/", nil)
		h.GetQuotaHistory(ctx)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"remaining_percent":23`) || !strings.Contains(rec.Body.String(), `"state":"completed"`) {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
}
