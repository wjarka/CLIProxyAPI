package management

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func historyEnabled(cfg *config.Config) bool {
	return cfg != nil && (cfg.QuotaHistory.Enabled == nil || *cfg.QuotaHistory.Enabled)
}
func historyStoragePath(cfg *config.Config, configPath string) (string, error) {
	if cfg == nil || configPath == "" {
		return "", errors.New("history needs a configuration path")
	}
	root, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return "", err
	}
	path := cfg.QuotaHistory.Path
	if path == "" {
		path = "quota-history.json"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if cfg.AuthDir != "" {
		auth := cfg.AuthDir
		if !filepath.IsAbs(auth) {
			auth = filepath.Join(root, auth)
		}
		// Resolve existing symlinks, including an existing parent for a new archive.
		var canonical func(string) string
		canonical = func(p string) string {
			if real, e := filepath.EvalSymlinks(p); e == nil {
				return real
			}
			parent := filepath.Dir(p)
			if parent == p {
				return p
			}
			return filepath.Join(canonical(parent), filepath.Base(p))
		}
		relative, err := filepath.Rel(canonical(auth), canonical(path))
		if err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))) {
			return "", errors.New("history must be outside auth directory")
		}
	}
	if cfg.QuotaHistory.RetentionWeeks < 0 || cfg.QuotaHistory.RetentionWeeks > 52 {
		return "", errors.New("history retention must be between 1 and 52 weeks")
	}
	return path, nil
}

func (h *Handler) StartQuotaHistory(ctx context.Context) {
	if h == nil {
		return
	}
	h.historyMu.Lock()
	defer h.historyMu.Unlock()
	if h.historyCancel != nil {
		return
	}
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	h.mu.Unlock()
	if !historyEnabled(cfg) {
		return
	}
	path, err := historyStoragePath(cfg, h.configFilePath)
	if err != nil {
		log.Warn("Weekly quota history unavailable: invalid storage configuration")
		return
	}
	retention := quotahistory.Retention
	if cfg.QuotaHistory.RetentionWeeks > 0 {
		retention = time.Duration(cfg.QuotaHistory.RetentionWeeks) * 7 * 24 * time.Hour
	}
	store, err := quotahistory.NewStore(path, retention)
	if err != nil {
		log.Warn("Weekly quota history archive could not be loaded; history storage is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lifetime, cancel := context.WithCancel(ctx)
	h.historyStore = store
	h.history = quotahistory.NewCollector(store, newQuotaHistorySource(h), time.Now)
	h.historyContext = lifetime
	h.historyCancel = cancel
	h.historyDone = make(chan struct{})
	collector, done := h.history, h.historyDone
	go func() { defer close(done); collector.Run(lifetime) }()
}
func (h *Handler) StopQuotaHistory() {
	if h == nil {
		return
	}
	h.historyMu.Lock()
	collector, done := h.history, h.historyDone
	if h.historyCancel != nil {
		h.historyCancel()
	}
	h.historyMu.Unlock()
	if done != nil {
		<-done
		collector.Wait()
	}
	h.historyMu.Lock()
	h.historyCancel = nil
	h.historyContext = nil
	h.historyDone = nil
	h.historyMu.Unlock()
}

func (h *Handler) GetQuotaHistory(c *gin.Context) {
	now := time.Now().UTC()
	from, to := now.Add(-7*24*time.Hour), now.Add(7*24*time.Hour)
	for key, dest := range map[string]*time.Time{"from": &from, "to": &to} {
		if raw := c.Query(key); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				c.JSON(400, gin.H{"error": "invalid history date range"})
				return
			}
			*dest = parsed
		}
	}
	if !to.After(from) || to.Sub(from) > quotahistory.Retention {
		c.JSON(400, gin.H{"error": "history range must be positive and at most 52 weeks"})
		return
	}
	h.historyMu.Lock()
	collector := h.history
	h.historyMu.Unlock()
	if collector == nil {
		c.JSON(200, quotahistory.Snapshot{Accounts: []quotahistory.Account{}, Cycles: []quotahistory.Cycle{}, Collection: []quotahistory.CollectionStatus{}, StorageStatus: "unavailable", ObservedAt: now})
		return
	}
	c.JSON(200, collector.Snapshot(from, to))
}
func (h *Handler) RefreshQuotaHistory(c *gin.Context) {
	var body struct {
		CredentialIDs []string `json:"credential_ids"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.CredentialIDs) > 1024 {
		c.JSON(400, gin.H{"error": "invalid history refresh request"})
		return
	}
	h.historyMu.Lock()
	collector, ctx := h.history, h.historyContext
	h.historyMu.Unlock()
	if collector == nil || ctx == nil {
		c.JSON(503, gin.H{"error": "quota history unavailable"})
		return
	}
	if err := collector.Refresh(ctx, body.CredentialIDs); err != nil {
		c.JSON(503, gin.H{"error": "history refresh unavailable"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "scheduled"})
}

func (h *Handler) observeHistoryAPICall(auth *coreauth.Auth, req *http.Request, requestBody string, responseBody []byte, observed time.Time) {
	if auth == nil || req == nil {
		return
	}
	h.historyMu.Lock()
	store := h.historyStore
	h.historyMu.Unlock()
	if store == nil {
		return
	}
	h.mu.Lock()
	manager := h.authManager
	enabled := historyEnabled(h.cfg)
	h.mu.Unlock()
	if manager == nil || !enabled {
		return
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Disabled || current.Status == coreauth.StatusDisabled || current.RegistrationEpoch != auth.RegistrationEpoch || historyAccountID(current) != historyAccountID(auth) {
		return
	}
	provider := historyProvider(auth)
	allowed := false
	if provider == "claude" {
		allowed = req.Method == http.MethodGet && req.URL.String() == coreauth.ClaudeUsageURL
	} else {
		token := tokenValueForAuth(auth)
		if provider == "meta" {
			token = historyValue(auth, "dca_token")
		}
		for _, expected := range historyQuotaRequests(auth, provider, token) {
			if req.Method != expected.method || req.URL.String() != expected.url {
				continue
			}
			if expected.headers["Authorization"] != "" && req.Header.Get("Authorization") != expected.headers["Authorization"] {
				continue
			}
			if provider == "devin" && gjson.Get(requestBody, "metadata.apiKey").String() != token {
				continue
			}
			if provider == "antigravity" && gjson.Get(requestBody, "project").String() != gjson.Get(expected.body, "project").String() {
				continue
			}
			allowed = true
			break
		}
	}
	if !allowed {
		return
	}
	observations, err := quotahistory.ParseWeekly(provider, historyAccountID(auth), responseBody, observed)
	if err != nil {
		return
	}
	for _, o := range observations {
		if err = store.Observe(o); err != nil {
			log.Warn("Weekly quota history could not persist a manual observation")
			break
		}
	}
}
