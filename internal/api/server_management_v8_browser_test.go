package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConsoleBrowserV8RoutesEnforceManagementAndHomeGates(t *testing.T) {
	t.Setenv("CLIPROXY_CONSOLE_BROWSER_URL", "")
	t.Setenv("MANAGEMENT_PASSWORD", "management-test")
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                         string
		enabled, home, authenticated bool
		rest, viewer                 int
	}{{"authorized", true, false, true, 200, 503}, {"no bearer", true, false, false, 401, 503}, {"disabled", false, false, true, 404, 404}, {"Home", true, true, true, 404, 404}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Home.Enabled = tc.home
			h := management.NewHandler(cfg, "", nil)
			h.SetLocalPassword("management-test")
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(tc.enabled)
			s.registerManagementV8Routes()
			for _, route := range []struct {
				method, path string
				want         int
			}{{"GET", "/v8/management/billing/browser", tc.rest}, {"GET", "/v8/management/billing/browser/sessions/id/view", tc.viewer}} {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
				request.RemoteAddr = "127.0.0.1:1234"
				if tc.authenticated {
					request.Header.Set("Authorization", "Bearer management-test")
				}
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, request)
				if w.Code != route.want {
					t.Fatalf("%s: %d %s", route.path, w.Code, w.Body.String())
				}
			}
		})
	}
}
