package management

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/zcode"
)

// RegisterZCodeRoutes mounts owner-only operations on the authenticated management group.
func (h *Handler) RegisterZCodeRoutes(management *gin.RouterGroup) {
	routes := management.Group("/zcode", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if h.zcode == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "zcode_storage_unavailable"})
		}
	})
	routes.GET("/account", func(c *gin.Context) {
		account, err := h.zcode.Account()
		writeZCodeResponse(c, account, err)
	})
	routes.DELETE("/account", func(c *gin.Context) {
		writeZCodeResponse(c, nil, h.zcode.Disconnect())
	})
	routes.POST("/login", func(c *gin.Context) {
		login, err := h.zcode.Start(c.Request.Context())
		writeZCodeResponse(c, login, err)
	})
	routes.GET("/login/:id", func(c *gin.Context) {
		status, err := h.zcode.Poll(c.Request.Context(), c.Param("id"))
		writeZCodeResponse(c, gin.H{"status": status}, err)
	})
	routes.DELETE("/login/:id", func(c *gin.Context) {
		h.zcode.Cancel(c.Param("id"))
		c.Status(http.StatusNoContent)
	})
	routes.GET("/reset-cards", func(c *gin.Context) {
		cards, err := h.zcode.Cards(c.Request.Context())
		writeZCodeResponse(c, cards, err)
	})
	routes.POST("/reset-cards/use", func(c *gin.Context) {
		var request zcode.UseRequest
		if !billingRequestJSON(c, &request) {
			return
		}
		err := h.zcode.Use(c.Request.Context(), request)
		writeZCodeResponse(c, gin.H{"used": true}, err)
	})
}

func writeZCodeResponse(c *gin.Context, result any, err error) {
	var status int
	var code string
	switch {
	case err == nil:
		if result == nil {
			c.Status(http.StatusNoContent)
			return
		}
		c.JSON(http.StatusOK, result)
		return
	case errors.Is(err, zcode.ErrNotConnected):
		status, code = http.StatusConflict, "zcode_not_connected"
	case errors.Is(err, zcode.ErrCardsChanged):
		status, code = http.StatusConflict, "zcode_cards_changed"
	case errors.Is(err, zcode.ErrReconnect):
		status, code = http.StatusConflict, "zcode_reconnect_required"
	case errors.Is(err, zcode.ErrInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, zcode.ErrStorage):
		status, code = http.StatusServiceUnavailable, "zcode_storage_unavailable"
	default:
		status, code = http.StatusBadGateway, "zcode_upstream_failed"
	}
	c.JSON(status, gin.H{"error": code})
}
