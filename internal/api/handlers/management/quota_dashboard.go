package management

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetQuotaOverview exposes only account labels, health and quota observations.
// Tokens, file paths and arbitrary auth metadata never enter this response.
func (h *Handler) GetQuotaOverview(c *gin.Context) {
	now := time.Now().UTC()
	rows := make([]gin.H, 0)
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth.Provider != "claude" && auth.Provider != "codex" {
				continue
			}
			unavailable, status, _, retry := reconcileAuthFileCooldownState(auth, now)
			label := auth.Label
			if label == "" {
				label = auth.ID
			}
			row := gin.H{"id": auth.ID, "label": label, "provider": auth.Provider, "disabled": auth.Disabled, "unavailable": unavailable, "status": status, "quota": quotaObservationPayload(auth.Quota)}
			if !retry.IsZero() {
				row["retry_at"] = retry
			}
			if reset := coreauth.WeeklyQuotaReset(auth, "", now); !reset.IsZero() {
				row["weekly_reset_at"] = reset
			}
			if auth.Provider == "claude" {
				if reset := coreauth.WeeklyQuotaReset(auth, "claude-fable", now); !reset.IsZero() {
					row["fable_reset_at"] = reset
				}
			}
			if auth.Provider == "codex" {
				enabled, _ := authWebsocketsValue(auth)
				row["websockets"] = enabled
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
	c.JSON(http.StatusOK, gin.H{"accounts": rows, "observed_at": now})
}
