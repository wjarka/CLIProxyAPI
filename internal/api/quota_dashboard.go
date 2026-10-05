package api

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed assets/quota.html
var quotaDashboard []byte

func (s *Server) serveQuotaDashboard(c *gin.Context) {
	if s.cfg == nil || s.cfg.Home.Enabled || s.cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	c.Data(http.StatusOK, "text/html; charset=utf-8", quotaDashboard)
}
