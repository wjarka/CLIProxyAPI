package api

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type consoleInjectionTransport struct{}

func (consoleInjectionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"amount":123,"currency":"USD"}`
	if r.URL.Path == "/api/organizations" {
		body = `[{"uuid":"org-injected","name":"Shared store","capabilities":["api"]}]`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}
func TestServerConsoleBillingUsesInjectedStore(t *testing.T) {
	manager, err := consolebilling.NewManager(t.TempDir(), &http.Client{Transport: consoleInjectionTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Connect(context.Background(), consolebilling.Claude, "shared-login", "fake-session"); err != nil {
		t.Fatal(err)
	}
	server := newTestServerWithOptions(t, WithConsoleBillingManager(manager))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/billing", nil)
	server.mgmt.GetConsoleBilling(c)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "org-injected") {
		t.Fatalf("server did not inject shared state: %d %s", recorder.Code, recorder.Body.String())
	}
}
