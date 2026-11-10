package management

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebrowser"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func browserRouter(h *Handler) *gin.Engine {
	r := gin.New()
	r.GET("/billing/browser", h.GetConsoleBrowser)
	r.POST("/billing/browser/sessions", h.CreateConsoleBrowserSession)
	r.GET("/billing/browser/sessions/:id", h.GetConsoleBrowserSession)
	r.DELETE("/billing/browser/sessions/:id", h.DeleteConsoleBrowserSession)
	r.POST("/billing/browser/sessions/:id/view-ticket", h.ConsoleBrowserViewTicket)
	r.GET("/billing/browser/sessions/:id/view", h.ConsoleBrowserView)
	r.POST("/billing/browser/sessions/:id/finish", h.FinishConsoleBrowserSession)
	return r
}
func browserRequest(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Origin", "https://dashboard.test")
	w := httptest.NewRecorder()
	browserRouter(h).ServeHTTP(w, r)
	return w
}
func TestConsoleBrowserDisabledContract(t *testing.T) {
	t.Setenv("CLIPROXY_CONSOLE_BROWSER_URL", "")
	h := newBillingTestHandler(t)
	w := browserRequest(h, "GET", "/billing/browser", "")
	if w.Code != 200 || w.Body.String() != `{"available":false,"max_sessions":3,"ttl_seconds":900}` || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected capability %d %s", w.Code, w.Body.String())
	}
	if w = browserRequest(h, "POST", "/billing/browser/sessions", `{"provider":"claude","label":"x"}`); w.Code != 503 {
		t.Fatalf("disabled status %d", w.Code)
	}
}
func TestConsoleBrowserValidationAndSessionContract(t *testing.T) {
	var calls atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer broker-secret" {
			t.Error("missing broker credential")
		}
		_ = json.NewEncoder(w).Encode(consolebrowser.Session{ID: "random-session-id", Provider: "claude", Status: "ready", ExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer b.Close()
	h := newBillingTestHandler(t)
	client, _ := consolebrowser.NewClient(b.URL, "broker-secret")
	h.consoleBrowser = newConsoleBrowserState(client)
	for _, body := range []string{`{"provider":"other","label":"x"}`, `{"provider":"claude"}`, `{"provider":"claude","label":"x","url":"https://evil.test"}`, `{"provider":"claude","connection_id":"missing"}`} {
		w := browserRequest(h, "POST", "/billing/browser/sessions", body)
		if w.Code < 400 || w.Code >= 500 {
			t.Fatalf("invalid request accepted %s: %d", body, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached broker")
	}
	w := browserRequest(h, "POST", "/billing/browser/sessions", `{"provider":"claude","label":"work"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "broker-secret") || strings.Contains(w.Body.String(), "work") {
		t.Fatalf("bad create response %d %s", w.Code, w.Body.String())
	}
	var s consolebrowser.Session
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if s.ID == "" || s.Status != "ready" {
		t.Fatal(w.Body.String())
	}
	w = browserRequest(h, "POST", "/billing/browser/sessions/"+s.ID+"/view-ticket", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ticket"`) {
		t.Fatalf("ticket %d %s", w.Code, w.Body.String())
	}
}

func browserFixture(t *testing.T, finish func(http.ResponseWriter, *http.Request)) (*Handler, *atomic.Int32) {
	t.Helper()
	deletes := new(atomic.Int32)
	var sequence atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-broker" || r.Header.Get("Cookie") != "" {
			t.Error("credential isolation failed")
		}
		switch {
		case r.Method == "DELETE":
			deletes.Add(1)
			w.Write([]byte(`{"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/finish"):
			finish(w, r)
		case strings.HasSuffix(r.URL.Path, "/view"):
			conn, err := (&websocket.Upgrader{Subprotocols: []string{"binary"}}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if conn.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")) != nil {
				return
			}
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if conn.WriteMessage(kind, data) != nil {
					return
				}
			}
		default:
			id := "broker-" + strconv.Itoa(int(sequence.Add(1)))
			_ = json.NewEncoder(w).Encode(consolebrowser.Session{ID: id, Provider: "claude", Status: "ready", ExpiresAt: time.Now().Add(10 * time.Minute)})
		}
	}))
	t.Cleanup(b.Close)
	h := newBillingTestHandler(t)
	client, _ := consolebrowser.NewClient(b.URL, "private-broker")
	h.consoleBrowser = newConsoleBrowserState(client)
	t.Cleanup(func() {
		h.consoleBrowser.mu.Lock()
		entries := make([]*consoleBrowserEntry, 0)
		for _, e := range h.consoleBrowser.sessions {
			entries = append(entries, e)
		}
		h.consoleBrowser.mu.Unlock()
		for _, e := range entries {
			e.mu.Lock()
			h.consoleBrowser.retireLocked(e, "not_found")
			e.mu.Unlock()
		}
	})
	return h, deletes
}
func createBrowserFixture(t *testing.T, h *Handler, body string) string {
	t.Helper()
	w := browserRequest(h, "POST", "/billing/browser/sessions", body)
	if w.Code != 200 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	var s consolebrowser.Session
	if json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatal(w.Body.String())
	}
	return s.ID
}
func browserTicketFixture(t *testing.T, h *Handler, id string) string {
	t.Helper()
	w := browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/view-ticket", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var v struct{ Ticket string }
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v.Ticket
}
func TestConsoleBrowserIncompleteAndFailedReconnectPreserveConnection(t *testing.T) {
	var ready atomic.Bool
	h, deletes := browserFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"login_not_ready"}`))
			return
		}
		w.Write([]byte(`{"session":"other-secret"}`))
	})
	h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"amount":500}`
		if r.URL.Path == "/api/organizations" {
			org := "org-one"
			if strings.Contains(r.Header.Get("Cookie"), "other-secret") {
				org = "org-other"
			}
			body = `[{"uuid":"` + org + `","capabilities":["api"]}]`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	manager, err := h.consoleBillingManager()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := manager.Connect(context.Background(), consolebilling.Claude, "existing", "first-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Bind("api-key", connection.AccountIDs[0], true); err != nil {
		t.Fatal(err)
	}
	before := manager.List()
	id := createBrowserFixture(t, h, `{"connection_id":"`+connection.ID+`"}`)
	for _, status := range []int{409, 409} {
		w := browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/finish", "")
		if w.Code != status {
			t.Fatalf("finish %d %s", w.Code, w.Body.String())
		}
		if !reflect.DeepEqual(before, manager.List()) || deletes.Load() != 0 {
			t.Fatal("failed login changed previous connection/browser")
		}
		ready.Store(true)
	}
	if w := browserRequest(h, "DELETE", "/billing/browser/sessions/"+id, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if !reflect.DeepEqual(before, manager.List()) || deletes.Load() != 1 {
		t.Fatal("cancel changed existing connection")
	}
}
func TestConsoleBrowserConcurrentFinishCommitsOnceThenCleansUp(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var extracts atomic.Int32
	h, deletes := browserFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if extracts.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.Write([]byte(`{"session":"saved-secret"}`))
	})
	h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"amount":500}`
		if r.URL.Path == "/api/organizations" {
			body = `[{"uuid":"org-one","capabilities":["api"]}]`
		}
		if deletes.Load() != 0 {
			t.Error("browser deleted before provider validated")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	id := createBrowserFixture(t, h, `{"provider":"claude","label":"new"}`)
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() { responses <- browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/finish", "") }()
	<-entered
	go func() { responses <- browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/finish", "") }()
	close(release)
	first, second := <-responses, <-responses
	if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() {
		t.Fatalf("duplicate finish %d %s / %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	manager, _ := h.consoleBillingManager()
	if len(manager.List().Connections) != 1 || extracts.Load() != 1 || deletes.Load() != 1 {
		t.Fatal("duplicate connection or missing cleanup")
	}
	if strings.Contains(first.Body.String(), "secret") {
		t.Fatal("session leaked")
	}
}
func TestConsoleBrowserTicketsBindOriginSessionExpireAndReplay(t *testing.T) {
	h, _ := browserFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) })
	id := createBrowserFixture(t, h, `{"provider":"claude","label":"x"}`)
	otherID := createBrowserFixture(t, h, `{"provider":"claude","label":"y"}`)
	server := httptest.NewServer(browserRouter(h))
	defer server.Close()
	base := "ws" + strings.TrimPrefix(server.URL, "http")
	dial := func(id, token, origin, query string) (*websocket.Conn, *http.Response, error) {
		d := websocket.Dialer{Subprotocols: []string{"binary", "cliproxy-ticket." + token}}
		return d.Dial(base+"/billing/browser/sessions/"+id+"/view"+query, http.Header{"Origin": []string{origin}})
	}
	ticket := browserTicketFixture(t, h, id)
	for _, tc := range []struct{ id, origin, query string }{{id, "https://evil.test", ""}, {otherID, "https://dashboard.test", ""}, {id, "https://dashboard.test", "?ticket=" + ticket}} {
		conn, response, err := dial(tc.id, ticket, tc.origin, tc.query)
		if conn != nil {
			conn.Close()
		}
		if err == nil || response.StatusCode != 400 {
			t.Fatal("invalid viewer ticket accepted")
		}
		if response != nil {
			response.Body.Close()
		}
	}
	conn, _, err := dial(id, ticket, "https://dashboard.test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.Subprotocol() != "binary" {
		t.Fatal("wrong selected protocol")
	}
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "RFB 003.008\n" {
		t.Fatal("missing broker RFB stream")
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, []byte("input")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.ReadMessage()
	if err != nil || string(data) != "input" {
		t.Fatal("viewer input not proxied")
	}
	replay, response, err := dial(id, ticket, "https://dashboard.test", "")
	if replay != nil {
		replay.Close()
	}
	if err == nil || response.StatusCode != 400 {
		t.Fatal("ticket replay accepted")
	}
	response.Body.Close()
	expired := browserTicketFixture(t, h, id)
	h.consoleBrowser.mu.Lock()
	ticketEntry := h.consoleBrowser.tickets[expired]
	ticketEntry.expires = time.Now().Add(-time.Second)
	h.consoleBrowser.tickets[expired] = ticketEntry
	h.consoleBrowser.mu.Unlock()
	_, response, err = dial(id, expired, "https://dashboard.test", "")
	if err == nil || response.StatusCode != 400 {
		t.Fatal("expired ticket accepted")
	}
	response.Body.Close()
	if w := browserRequest(h, "DELETE", "/billing/browser/sessions/"+id, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, _, err = conn.ReadMessage(); err == nil {
		t.Fatal("cancel failed to close viewer")
	}
}
func TestConsoleBrowserBoundsSessionsAndExpiresWithoutSaving(t *testing.T) {
	h, deletes := browserFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("expired browser extracted") })
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = createBrowserFixture(t, h, `{"provider":"claude","label":"x"}`)
	}
	if w := browserRequest(h, "POST", "/billing/browser/sessions", `{"provider":"claude","label":"overflow"}`); w.Code != 429 {
		t.Fatalf("unbounded sessions: %d", w.Code)
	}
	h.consoleBrowser.mu.Lock()
	entry := h.consoleBrowser.sessions[ids[0]]
	h.consoleBrowser.mu.Unlock()
	entry.mu.Lock()
	entry.session.ExpiresAt = time.Now().Add(-time.Second)
	entry.mu.Unlock()
	if w := browserRequest(h, "POST", "/billing/browser/sessions/"+ids[0]+"/finish", ""); w.Code != 410 {
		t.Fatalf("expired finish %d", w.Code)
	}
	manager, _ := h.consoleBillingManager()
	if deletes.Load() != 1 || len(manager.List().Connections) != 0 {
		t.Fatal("expiry did not clean browser only")
	}
	_ = createBrowserFixture(t, h, `{"provider":"claude","label":"replacement"}`)
}
func TestConsoleBrowserNewViewerClosesPreviousSocket(t *testing.T) {
	h, _ := browserFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) })
	id := createBrowserFixture(t, h, `{"provider":"claude","label":"x"}`)
	server := httptest.NewServer(browserRouter(h))
	defer server.Close()
	connect := func() *websocket.Conn {
		ticket := browserTicketFixture(t, h, id)
		d := websocket.Dialer{Subprotocols: []string{"binary", "cliproxy-ticket." + ticket}}
		conn, _, err := d.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/billing/browser/sessions/"+id+"/view", http.Header{"Origin": []string{"https://dashboard.test"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	first := connect()
	defer first.Close()
	second := connect()
	defer second.Close()
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err := first.ReadMessage()
	if err == nil {
		t.Fatal("previous viewer stayed active")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("new viewer failed to revoke old socket")
	}
}

func TestConsoleBrowserSuccessfulReconnectPreservesIdentities(t *testing.T) {
	h, deletes := browserFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"session":"new-session-secret"}`)) })
	h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"amount":500}`
		if r.URL.Path == "/api/organizations" {
			body = `[{"uuid":"org-one","capabilities":["api"]}]`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	manager, _ := h.consoleBillingManager()
	connection, err := manager.Connect(context.Background(), consolebilling.Claude, "existing", "first-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Bind("key", connection.AccountIDs[0], true); err != nil {
		t.Fatal(err)
	}
	before := manager.List()
	for _, body := range []string{`{"provider":"openai","connection_id":"` + connection.ID + `"}`, `{"label":"other","connection_id":"` + connection.ID + `"}`} {
		if w := browserRequest(h, "POST", "/billing/browser/sessions", body); w.Code != 400 {
			t.Fatal("reconnect mismatch accepted")
		}
	}
	id := createBrowserFixture(t, h, `{"connection_id":"`+connection.ID+`"}`)
	w := browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/finish", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("finish %d %s", w.Code, w.Body.String())
	}
	after := manager.List()
	if len(after.Connections) != 1 || after.Connections[0].ID != connection.ID || after.Accounts[0].ID != before.Accounts[0].ID || !reflect.DeepEqual(before.Bindings, after.Bindings) || deletes.Load() != 1 {
		t.Fatal("reconnect lost identities or bindings")
	}
}
func TestConsoleBrowserCancelBeforeFinishCannotSave(t *testing.T) {
	h, deletes := browserFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("canceled browser extracted") })
	id := createBrowserFixture(t, h, `{"provider":"claude","label":"x"}`)
	ticket := browserTicketFixture(t, h, id)
	if w := browserRequest(h, "DELETE", "/billing/browser/sessions/"+id, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/finish", ""); w.Code != 404 {
		t.Fatal("canceled browser finished")
	}
	h.consoleBrowser.mu.Lock()
	_, valid := h.consoleBrowser.tickets[ticket]
	h.consoleBrowser.mu.Unlock()
	manager, _ := h.consoleBillingManager()
	if valid || deletes.Load() != 1 || len(manager.List().Connections) != 0 {
		t.Fatal("cancel left ticket or connection")
	}
}
func TestConsoleBrowserCancelInterruptsFinishBeforeSaving(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h, deletes := browserFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.Write([]byte(`{"session":"must-not-save"}`))
	})
	h.billingHTTPClient = &http.Client{Transport: billingTestTransport(func(r *http.Request) (*http.Response, error) {
		t.Error("canceled finish reached billing provider")
		return nil, errors.New("must not request")
	})}
	id := createBrowserFixture(t, h, `{"provider":"claude","label":"x"}`)
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- browserRequest(h, "POST", "/billing/browser/sessions/"+id+"/finish", "") }()
	<-entered
	canceled := make(chan *httptest.ResponseRecorder, 1)
	go func() { canceled <- browserRequest(h, "DELETE", "/billing/browser/sessions/"+id, "") }()
	select {
	case w := <-canceled:
		close(release)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	case <-time.After(time.Second):
		close(release)
		<-finished
		<-canceled
		t.Fatal("cancel waited for finish instead of interrupting it")
	}
	w := <-finished
	if w.Code == 200 {
		t.Fatal("canceled finish succeeded")
	}
	manager, _ := h.consoleBillingManager()
	if len(manager.List().Connections) != 0 || deletes.Load() != 1 {
		t.Fatal("cancel failed to preserve connection state")
	}
}
