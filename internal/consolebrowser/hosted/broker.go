// Package hosted implements the private, ephemeral Console login browser broker.
package hosted

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const MaxSessions = 3
const TTL = 15 * time.Minute

var ErrNotReady = errors.New("login_not_ready")

type Session struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Instance interface {
	VNCAddr() string
	Done() <-chan struct{}
	Finish(context.Context, string) (string, error)
	Close()
}
type Controller interface {
	Start(context.Context, string) (Instance, error)
}
type Config struct {
	Token       string
	ProfileRoot string
	Controller  Controller
	Now         func() time.Time
}
type liveSession struct {
	Session
	instance Instance
	cancel   context.CancelFunc
	finish   sync.Mutex
}
type Broker struct {
	mu         sync.Mutex
	starts     sync.WaitGroup
	sessions   map[string]*liveSession
	expired    map[string]bool
	token      [32]byte
	controller Controller
	now        func() time.Time
	closed     bool
	stop       chan struct{}
	done       chan struct{}
}

func New(c Config) (*Broker, error) {
	if len(c.Token) < 32 || len(c.Token) > 256 || strings.ContainsAny(c.Token, " \t\r\n") {
		return nil, errors.New("invalid broker token")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Controller == nil {
		var err error
		c.Controller, err = NewProcessController(c.ProfileRoot)
		if err != nil {
			return nil, err
		}
	}
	b := &Broker{sessions: make(map[string]*liveSession), expired: make(map[string]bool), token: sha256.Sum256([]byte(c.Token)), controller: c.Controller, now: c.Now, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(b.done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				b.sweep()
			case <-b.stop:
				return
			}
		}
	}()
	return b, nil
}
func (b *Broker) Close() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		close(b.stop)
		for id, s := range b.sessions {
			b.removeLocked(id, s, false)
		}
	}
	b.mu.Unlock()
	b.starts.Wait()
	<-b.done
}
func (b *Broker) removeLocked(id string, s *liveSession, expired bool) {
	delete(b.sessions, id)
	if expired {
		if len(b.expired) >= 1024 {
			b.expired = make(map[string]bool)
		}
		b.expired[id] = true
	}
	s.cancel()
	if s.instance != nil {
		s.instance.Close()
	}
}
func (b *Broker) sweep() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, s := range b.sessions {
		if !b.now().Before(s.ExpiresAt) {
			b.removeLocked(id, s, true)
			continue
		}
		if s.instance != nil {
			select {
			case <-s.instance.Done():
				b.removeLocked(id, s, false)
			default:
			}
		}
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || len(auth[0]) > 263 || !strings.HasPrefix(auth[0], "Bearer ") {
		fail(w, 401, "invalid_request")
		return
	}
	hash := sha256.Sum256([]byte(strings.TrimPrefix(auth[0], "Bearer ")))
	if subtle.ConstantTimeCompare(hash[:], b.token[:]) != 1 {
		fail(w, 401, "invalid_request")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, 400, "invalid_request")
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/sessions" && r.Method == "POST" {
		b.create(w, r)
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(p) < 2 || len(p) > 3 || p[0] != "sessions" || len(p[1]) != 43 {
		fail(w, 404, "not_found")
		return
	}
	id := p[1]
	b.sweep()
	b.mu.Lock()
	s := b.sessions[id]
	ready := s != nil && s.instance != nil
	expired := b.expired[id]
	b.mu.Unlock()
	if r.Method == "DELETE" && len(p) == 2 {
		b.mu.Lock()
		if current := b.sessions[id]; current != nil {
			b.removeLocked(id, current, false)
		}
		b.mu.Unlock()
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if !ready {
		if expired {
			fail(w, 410, "browser_expired")
		} else {
			fail(w, 404, "not_found")
		}
		return
	}
	if len(p) == 2 && r.Method == "GET" {
		writeJSON(w, 200, s.Session)
		return
	}
	if len(p) == 3 && p[2] == "finish" && r.Method == "POST" {
		if !s.finish.TryLock() {
			fail(w, 409, "login_not_ready")
			return
		}
		defer s.finish.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		token, err := s.instance.Finish(ctx, s.Provider)
		if err != nil || token == "" {
			fail(w, 409, "login_not_ready")
			return
		}
		b.mu.Lock()
		active := b.sessions[id] == s && b.now().Before(s.ExpiresAt)
		b.mu.Unlock()
		if !active {
			fail(w, 410, "browser_expired")
			return
		}
		writeJSON(w, 200, map[string]string{"session": token})
		return
	}
	if len(p) == 3 && p[2] == "view" && r.Method == "GET" {
		b.view(w, r, s)
		return
	}
	fail(w, 404, "not_found")
}
func (b *Broker) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF || (body.Provider != "claude" && body.Provider != "openai") {
		fail(w, 400, "invalid_request")
		return
	}
	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		fail(w, 503, "browser_unavailable")
		return
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	b.sweep()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		fail(w, 503, "browser_unavailable")
		return
	}
	if len(b.sessions) >= MaxSessions {
		b.mu.Unlock()
		fail(w, 429, "browser_busy")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &liveSession{Session: Session{ID: id, Provider: body.Provider, Status: "ready", ExpiresAt: b.now().Add(TTL)}, cancel: cancel}
	b.sessions[id] = s
	b.starts.Add(1)
	b.mu.Unlock()
	defer b.starts.Done()
	startCtx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	stopOnDisconnect := context.AfterFunc(r.Context(), stop)
	defer stopOnDisconnect()
	p, err := b.controller.Start(startCtx, body.Provider)
	b.mu.Lock()
	if err != nil || b.closed || b.sessions[id] != s || r.Context().Err() != nil {
		if b.sessions[id] == s {
			b.removeLocked(id, s, false)
		}
		b.mu.Unlock()
		if p != nil {
			p.Close()
		}
		fail(w, 503, "browser_unavailable")
		return
	}
	s.instance = p
	b.mu.Unlock()
	writeJSON(w, 201, s.Session)
}
func (b *Broker) view(w http.ResponseWriter, r *http.Request, s *liveSession) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	vnc, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.instance.VNCAddr())
	cancel()
	if err != nil {
		fail(w, 503, "browser_unavailable")
		return
	}
	defer vnc.Close()
	up := websocket.Upgrader{Subprotocols: []string{"binary"}, CheckOrigin: func(*http.Request) bool { return true }, ReadBufferSize: 16384, WriteBufferSize: 16384}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1024 * 1024)
	finished := make(chan struct{})
	go func() {
		select {
		case <-s.instance.Done():
			_ = ws.Close()
			_ = vnc.Close()
		case <-finished:
		}
	}()
	defer close(finished)
	go func() {
		defer ws.Close()
		buf := make([]byte, 32768)
		for {
			n, e := vnc.Read(buf)
			if n > 0 {
				if ws.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	for {
		kind, data, e := ws.ReadMessage()
		if e != nil || kind != websocket.BinaryMessage {
			return
		}
		if _, e = vnc.Write(data); e != nil {
			return
		}
	}
}
