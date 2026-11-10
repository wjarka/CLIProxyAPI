package management

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebrowser"
)

const browserTTL = 15 * time.Minute

type consoleBrowserEntry struct {
	cancelMu                                sync.Mutex
	cancel                                  context.CancelFunc
	cancelled                               bool
	mu                                      sync.Mutex
	session                                 consolebrowser.Session
	brokerID, label, connectionID, resultID string
	closed                                  bool
	timer                                   *time.Timer
	viewers                                 map[*websocket.Conn]*websocket.Conn
}
type consoleBrowserTicket struct {
	sessionID, origin string
	expires           time.Time
}
type consoleBrowserTombstone struct {
	code, resultID string
	expires        time.Time
}
type consoleBrowserState struct {
	client     *consolebrowser.Client
	mu         sync.Mutex
	sessions   map[string]*consoleBrowserEntry
	tickets    map[string]consoleBrowserTicket
	tombstones map[string]consoleBrowserTombstone
	creating   int
	now        func() time.Time
}

func newConsoleBrowserState(client *consolebrowser.Client) *consoleBrowserState {
	return &consoleBrowserState{client: client, sessions: make(map[string]*consoleBrowserEntry), tickets: make(map[string]consoleBrowserTicket), tombstones: make(map[string]consoleBrowserTombstone), now: time.Now}
}
func browserOpaque() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func browserError(c *gin.Context, code string) {
	status := map[string]int{"browser_unavailable": 503, "browser_busy": 429, "browser_expired": 410, "login_not_ready": 409, "invalid_request": 400, "not_found": 404}[code]
	if status == 0 {
		code, status = "browser_unavailable", 503
	}
	c.JSON(status, gin.H{"error": code})
}
func browserOperationError(c *gin.Context, err error) {
	var safe *consolebrowser.Error
	if errors.As(err, &safe) {
		browserError(c, safe.Code)
		return
	}
	browserError(c, "browser_unavailable")
}
func (h *Handler) browserForRequest(c *gin.Context) *consoleBrowserState {
	c.Header("Cache-Control", "no-store")
	if h.consoleBrowser == nil {
		browserError(c, "browser_unavailable")
		return nil
	}
	return h.consoleBrowser
}
func (h *Handler) GetConsoleBrowser(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"available": h.consoleBrowser != nil, "max_sessions": 3, "ttl_seconds": 900})
}
func (s *consoleBrowserState) pruneLocked() {
	now := s.now()
	for id, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, id)
		}
	}
	for id, t := range s.tombstones {
		if !now.Before(t.expires) {
			delete(s.tombstones, id)
		}
	}
}

// retireLocked linearizes cancellation/expiry with Finish. The entry mutex must be held.
func (s *consoleBrowserState) retireLocked(e *consoleBrowserEntry, code string) {
	if e.closed {
		return
	}
	e.closed = true
	if e.timer != nil {
		e.timer.Stop()
	}
	for local, remote := range e.viewers {
		_ = local.Close()
		_ = remote.Close()
	}
	e.viewers = nil
	s.mu.Lock()
	delete(s.sessions, e.session.ID)
	for token, t := range s.tickets {
		if t.sessionID == e.session.ID {
			delete(s.tickets, token)
		}
	}
	s.pruneLocked()
	if len(s.tombstones) >= 128 {
		var oldest string
		var at time.Time
		for id, t := range s.tombstones {
			if oldest == "" || t.expires.Before(at) {
				oldest, at = id, t.expires
			}
		}
		delete(s.tombstones, oldest)
	}
	s.tombstones[e.session.ID] = consoleBrowserTombstone{code: code, resultID: e.resultID, expires: s.now().Add(browserTTL)}
	s.mu.Unlock()
	// A failed DELETE never revives a local session; the broker enforces its own TTL.
	_ = s.client.Delete(context.Background(), e.brokerID)
}
func (s *consoleBrowserState) entry(c *gin.Context) *consoleBrowserEntry {
	id := c.Param("id")
	if !consolebrowser.ValidID(id) {
		browserError(c, "not_found")
		return nil
	}
	s.mu.Lock()
	s.pruneLocked()
	e := s.sessions[id]
	t, known := s.tombstones[id]
	s.mu.Unlock()
	if e == nil {
		if known && t.resultID != "" && strings.HasSuffix(c.Request.URL.Path, "/finish") {
			c.JSON(200, gin.H{"status": "ok", "connection_id": t.resultID})
			return nil
		}
		if known && t.code == "browser_expired" {
			browserError(c, "browser_expired")
		} else {
			browserError(c, "not_found")
		}
		return nil
	}
	e.mu.Lock()
	e.cancelMu.Lock()
	cancelled := e.cancelled
	e.cancelMu.Unlock()
	if cancelled && !e.closed {
		s.retireLocked(e, "not_found")
	}
	if e.closed {
		if e.resultID != "" && strings.HasSuffix(c.Request.URL.Path, "/finish") {
			c.JSON(200, gin.H{"status": "ok", "connection_id": e.resultID})
		} else {
			browserError(c, "not_found")
		}
		e.mu.Unlock()
		return nil
	}
	if !s.now().Before(e.session.ExpiresAt) {
		s.retireLocked(e, "browser_expired")
		e.mu.Unlock()
		browserError(c, "browser_expired")
		return nil
	}
	return e
}
func (h *Handler) CreateConsoleBrowserSession(c *gin.Context) {
	s := h.browserForRequest(c)
	if s == nil {
		return
	}
	var input struct {
		Provider     string `json:"provider"`
		Label        string `json:"label"`
		ConnectionID string `json:"connection_id"`
	}
	if !billingRequestJSON(c, &input) {
		return
	}
	if (input.Provider != "" && input.Provider != "claude" && input.Provider != "openai") || len(input.Label) > 512 {
		browserError(c, "invalid_request")
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	if input.ConnectionID != "" {
		found := false
		for _, connection := range manager.List().Connections {
			if connection.ID == input.ConnectionID {
				found = true
				if (input.Provider != "" && input.Provider != string(connection.Provider)) || (input.Label != "" && input.Label != connection.Label) {
					browserError(c, "invalid_request")
					return
				}
				input.Provider, input.Label = string(connection.Provider), connection.Label
				break
			}
		}
		if !found {
			browserError(c, "not_found")
			return
		}
	} else if strings.TrimSpace(input.Label) == "" || (input.Provider != "claude" && input.Provider != "openai") {
		browserError(c, "invalid_request")
		return
	}
	// Expired entries are retired before reserving capacity; network creation occurs outside the map lock.
	s.mu.Lock()
	entries := make([]*consoleBrowserEntry, 0, len(s.sessions))
	for _, e := range s.sessions {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		if !s.now().Before(e.session.ExpiresAt) {
			s.retireLocked(e, "browser_expired")
		}
		e.mu.Unlock()
	}
	s.mu.Lock()
	if len(s.sessions)+s.creating >= 3 {
		s.mu.Unlock()
		browserError(c, "browser_busy")
		return
	}
	s.creating++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.creating--; s.mu.Unlock() }()
	upstream, err := s.client.Create(c.Request.Context(), input.Provider)
	if err != nil {
		browserOperationError(c, err)
		return
	}
	id, err := browserOpaque()
	if err != nil {
		_ = s.client.Delete(context.Background(), upstream.ID)
		browserError(c, "browser_unavailable")
		return
	}
	expires := upstream.ExpiresAt
	if limit := s.now().Add(browserTTL); expires.After(limit) {
		expires = limit
	}
	if !s.now().Before(expires) {
		_ = s.client.Delete(context.Background(), upstream.ID)
		browserError(c, "browser_expired")
		return
	}
	e := &consoleBrowserEntry{session: consolebrowser.Session{ID: id, Provider: input.Provider, Status: "ready", ExpiresAt: expires}, brokerID: upstream.ID, label: input.Label, connectionID: input.ConnectionID, viewers: make(map[*websocket.Conn]*websocket.Conn)}
	e.mu.Lock()
	s.mu.Lock()
	s.sessions[id] = e
	s.mu.Unlock()
	e.timer = time.AfterFunc(expires.Sub(s.now()), func() { e.mu.Lock(); defer e.mu.Unlock(); s.retireLocked(e, "browser_expired") })
	e.mu.Unlock()
	c.JSON(200, e.session)
}
func (h *Handler) GetConsoleBrowserSession(c *gin.Context) {
	s := h.browserForRequest(c)
	if s == nil {
		return
	}
	e := s.entry(c)
	if e == nil {
		return
	}
	defer e.mu.Unlock()
	upstream, err := s.client.Get(c.Request.Context(), e.brokerID)
	if err != nil {
		var safe *consolebrowser.Error
		if errors.As(err, &safe) && (safe.Code == "browser_expired" || safe.Code == "not_found") {
			s.retireLocked(e, "browser_expired")
			browserError(c, "browser_expired")
			return
		}
		browserOperationError(c, err)
		return
	}
	if upstream.Provider != e.session.Provider {
		browserError(c, "browser_unavailable")
		return
	}
	c.JSON(200, e.session)
}
func (h *Handler) DeleteConsoleBrowserSession(c *gin.Context) {
	s := h.browserForRequest(c)
	if s == nil {
		return
	}
	s.mu.Lock()
	e := s.sessions[c.Param("id")]
	s.mu.Unlock()
	if e != nil {
		e.cancelMu.Lock()
		e.cancelled = true
		if e.cancel != nil {
			e.cancel()
		}
		e.cancelMu.Unlock()
		e.mu.Lock()
		s.retireLocked(e, "not_found")
		e.mu.Unlock()
	}
	c.JSON(200, gin.H{"status": "ok"})
}
func validBrowserOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && origin == u.Scheme+"://"+u.Host
}
func (h *Handler) ConsoleBrowserViewTicket(c *gin.Context) {
	s := h.browserForRequest(c)
	if s == nil {
		return
	}
	origin := c.GetHeader("Origin")
	if !validBrowserOrigin(origin) {
		browserError(c, "invalid_request")
		return
	}
	e := s.entry(c)
	if e == nil {
		return
	}
	defer e.mu.Unlock()
	token, err := browserOpaque()
	if err != nil {
		browserError(c, "browser_unavailable")
		return
	}
	s.mu.Lock()
	s.pruneLocked()
	for key, t := range s.tickets {
		if t.sessionID == e.session.ID {
			delete(s.tickets, key)
		}
	}
	s.tickets[token] = consoleBrowserTicket{sessionID: e.session.ID, origin: origin, expires: s.now().Add(30 * time.Second)}
	s.mu.Unlock()
	c.JSON(200, gin.H{"ticket": token})
}
func (h *Handler) FinishConsoleBrowserSession(c *gin.Context) {
	s := h.browserForRequest(c)
	if s == nil {
		return
	}
	e := s.entry(c)
	if e == nil {
		return
	}
	defer e.mu.Unlock()
	ctx, cancel := context.WithDeadline(c.Request.Context(), e.session.ExpiresAt)
	e.cancelMu.Lock()
	if e.cancelled {
		e.cancelMu.Unlock()
		cancel()
		browserError(c, "not_found")
		return
	}
	e.cancel = cancel
	e.cancelMu.Unlock()
	defer func() { cancel(); e.cancelMu.Lock(); e.cancel = nil; e.cancelMu.Unlock() }()
	secret, err := s.client.Finish(ctx, e.brokerID)
	if err != nil {
		if !s.now().Before(e.session.ExpiresAt) {
			s.retireLocked(e, "browser_expired")
			browserError(c, "browser_expired")
			return
		}
		browserOperationError(c, err)
		return
	}
	if !s.now().Before(e.session.ExpiresAt) {
		secret = ""
		s.retireLocked(e, "browser_expired")
		browserError(c, "browser_expired")
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	var connection consolebilling.Connection
	if e.connectionID != "" {
		connection, err = manager.Reconnect(ctx, e.connectionID, secret)
	} else {
		connection, err = manager.Connect(ctx, consolebilling.Provider(e.session.Provider), e.label, secret)
	}
	secret = ""
	if err != nil {
		if !s.now().Before(e.session.ExpiresAt) {
			s.retireLocked(e, "browser_expired")
			browserError(c, "browser_expired")
			return
		}
		consoleBillingOperationError(c, err)
		return
	}
	e.resultID = connection.ID
	s.retireLocked(e, "not_found")
	c.JSON(200, gin.H{"status": "ok", "connection_id": connection.ID})
}
func (h *Handler) ConsoleBrowserView(c *gin.Context) {
	s := h.browserForRequest(c)
	if s == nil {
		return
	}
	origin := c.GetHeader("Origin")
	if !validBrowserOrigin(origin) || c.Request.URL.RawQuery != "" {
		browserError(c, "invalid_request")
		return
	}
	var token string
	binary := false
	for _, protocol := range websocket.Subprotocols(c.Request) {
		if protocol == "binary" {
			binary = true
		} else if strings.HasPrefix(protocol, "cliproxy-ticket.") && token == "" {
			token = strings.TrimPrefix(protocol, "cliproxy-ticket.")
		} else {
			browserError(c, "invalid_request")
			return
		}
	}
	if !binary || token == "" {
		browserError(c, "invalid_request")
		return
	}
	s.mu.Lock()
	s.pruneLocked()
	ticket, ok := s.tickets[token]
	if ok && ticket.origin == origin && ticket.sessionID == c.Param("id") {
		delete(s.tickets, token)
	} else {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		browserError(c, "invalid_request")
		return
	}
	e := s.entry(c)
	if e == nil {
		return
	}
	upstream, err := s.client.View(c.Request.Context(), e.brokerID)
	if err != nil {
		e.mu.Unlock()
		browserOperationError(c, err)
		return
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{"binary"}, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == ticket.origin }, Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	}}
	local, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		_ = upstream.Close()
		e.mu.Unlock()
		return
	}
	local.SetReadLimit(4 << 20)
	upstream.SetReadLimit(4 << 20)
	for previous, remote := range e.viewers {
		_ = previous.Close()
		_ = remote.Close()
		delete(e.viewers, previous)
	}
	e.viewers[local] = upstream
	e.mu.Unlock()
	done := make(chan struct{}, 2)
	copyFrames := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			kind, data, err := src.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage {
				return
			}
			if dst.WriteMessage(websocket.BinaryMessage, data) != nil {
				return
			}
		}
	}
	go copyFrames(upstream, local)
	go copyFrames(local, upstream)
	<-done
	_ = local.Close()
	_ = upstream.Close()
	<-done
	e.mu.Lock()
	delete(e.viewers, local)
	e.mu.Unlock()
}
