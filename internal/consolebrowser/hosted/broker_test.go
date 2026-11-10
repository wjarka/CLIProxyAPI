package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeInstance struct {
	addr string
	done chan struct{}
	once sync.Once
}

func (p *fakeInstance) VNCAddr() string                                { return p.addr }
func (p *fakeInstance) Done() <-chan struct{}                          { return p.done }
func (p *fakeInstance) Close()                                         { p.once.Do(func() { close(p.done) }) }
func (p *fakeInstance) Finish(context.Context, string) (string, error) { return "console-session", nil }

type fakeController struct {
	instances []*fakeInstance
	mu        sync.Mutex
	addr      string
}

func (c *fakeController) Start(context.Context, string) (Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := &fakeInstance{addr: c.addr, done: make(chan struct{})}
	c.instances = append(c.instances, p)
	return p, nil
}
func setup(t *testing.T, now func() time.Time) (*Broker, *fakeController) {
	t.Helper()
	c := &fakeController{}
	b, err := New(Config{Token: strings.Repeat("x", 32), Controller: c, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b, c
}
func req(b http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	return w
}
func create(t *testing.T, b *Broker) Session {
	t.Helper()
	w := req(b, "POST", "/sessions", `{"provider":"claude"}`, strings.Repeat("x", 32))
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var s Session
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func TestAuthAndValidation(t *testing.T) {
	b, _ := setup(t, time.Now)
	for _, p := range []string{"/healthz", "/sessions", "/sessions/random/view"} {
		if w := req(b, "GET", p, "", ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", p, w.Code)
		}
	}
	for _, body := range []string{`{"provider":"google"}`, `{"provider":"claude","url":"http://localhost"}`, strings.Repeat("x", 5000)} {
		w := req(b, "POST", "/sessions", body, strings.Repeat("x", 32))
		if w.Code != 400 {
			t.Fatalf("invalid: %d", w.Code)
		}
	}
	w := req(b, "GET", "/healthz", "", strings.Repeat("x", 32))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
}
func TestLimitIndependentExpiryAndCleanup(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, c := setup(t, func() time.Time { return now })
	a := create(t, b)
	d := create(t, b)
	create(t, b)
	if a.ID == d.ID || len(a.ID) < 32 || a.ExpiresAt.Sub(now) != 15*time.Minute {
		t.Fatalf("bad sessions %#v %#v", a, d)
	}
	if w := req(b, "POST", "/sessions", `{"provider":"claude"}`, strings.Repeat("x", 32)); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if w := req(b, "DELETE", "/sessions/"+a.ID, "", strings.Repeat("x", 32)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	select {
	case <-c.instances[0].done:
	default:
		t.Fatal("cancel did not close")
	}
	select {
	case <-c.instances[1].done:
		t.Fatal("cancel closed other session")
	default:
	}
	now = now.Add(15 * time.Minute)
	b.sweep()
	w := req(b, "GET", "/sessions/"+d.ID, "", strings.Repeat("x", 32))
	if w.Code != 410 {
		t.Fatal(w.Code)
	}
	for _, p := range c.instances {
		select {
		case <-p.done:
		default:
			t.Fatal("expiry did not close")
		}
	}
}
func TestFinishDoesNotCloseAndShutdownCleans(t *testing.T) {
	b, c := setup(t, time.Now)
	s := create(t, b)
	w := req(b, "POST", "/sessions/"+s.ID+"/finish", "", strings.Repeat("x", 32))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"session":"console-session"`)) {
		t.Fatal(w.Code, w.Body)
	}
	select {
	case <-c.instances[0].done:
		t.Fatal("finish closed before save")
	default:
	}
	b.Close()
	select {
	case <-c.instances[0].done:
	default:
		t.Fatal("shutdown did not close")
	}
}
func TestViewCarriesBinaryRFBAndClosesOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan string, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("RFB 003.008\n"))
		buf := make([]byte, 4)
		_, _ = io.ReadFull(c, buf)
		received <- string(buf)
		_, _ = io.Copy(io.Discard, c)
	}()
	b, controller := setup(t, time.Now)
	controller.addr = ln.Addr().String()
	s := create(t, b)
	server := httptest.NewServer(b)
	defer server.Close()
	h := http.Header{"Authorization": []string{"Bearer " + strings.Repeat("x", 32)}}
	ws, _, err := (&websocket.Dialer{Subprotocols: []string{"binary"}}).Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/sessions/"+s.ID+"/view", h)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	kind, msg, err := ws.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(msg) != "RFB 003.008\n" {
		t.Fatal(kind, string(msg), err)
	}
	if err = ws.WriteMessage(websocket.BinaryMessage, []byte("keys")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "keys" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("input lost")
	}
	req(b, "DELETE", "/sessions/"+s.ID, "", strings.Repeat("x", 32))
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = ws.ReadMessage(); err == nil {
		t.Fatal("cancel left viewer open")
	}
}

type blockingController struct {
	started chan struct{}
	stopped chan struct{}
}

func (c *blockingController) Start(ctx context.Context, _ string) (Instance, error) {
	close(c.started)
	<-ctx.Done()
	close(c.stopped)
	return nil, ctx.Err()
}
func TestCancelledCreateStopsStartup(t *testing.T) {
	c := &blockingController{started: make(chan struct{}), stopped: make(chan struct{})}
	b, err := New(Config{Token: strings.Repeat("x", 32), Controller: c})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/sessions", strings.NewReader(`{"provider":"claude"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	ended := make(chan struct{})
	go func() { b.ServeHTTP(httptest.NewRecorder(), r); close(ended) }()
	<-c.started
	cancel()
	select {
	case <-c.stopped:
	case <-time.After(time.Second):
		t.Fatal("disconnected create left startup running")
	}
	<-ended
}

func TestShutdownWaitsForInFlightStartup(t *testing.T) {
	c := &blockingController{started: make(chan struct{}), stopped: make(chan struct{})}
	b, err := New(Config{Token: strings.Repeat("x", 32), Controller: c})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { req(b, "POST", "/sessions", `{"provider":"claude"}`, strings.Repeat("x", 32)); close(ended) }()
	<-c.started
	b.Close()
	select {
	case <-c.stopped:
	default:
		t.Fatal("shutdown returned with startup resources still running")
	}
	<-ended
}

type slowFinish struct {
	fakeInstance
	entered chan struct{}
}

func (p *slowFinish) Finish(ctx context.Context, _ string) (string, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

type fixedController struct{ p Instance }

func (c fixedController) Start(context.Context, string) (Instance, error) { return c.p, nil }
func TestConcurrentFinishRejectsInsteadOfQueuing(t *testing.T) {
	p := &slowFinish{fakeInstance: fakeInstance{done: make(chan struct{})}, entered: make(chan struct{}, 1)}
	b, err := New(Config{Token: strings.Repeat("x", 32), Controller: fixedController{p: p}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s := create(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/sessions/"+s.ID+"/finish", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	done := make(chan struct{})
	go func() { b.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	<-p.entered
	second := make(chan int, 1)
	go func() { second <- req(b, "POST", "/sessions/"+s.ID+"/finish", "", strings.Repeat("x", 32)).Code }()
	select {
	case status := <-second:
		if status != 409 {
			t.Fatal(status)
		}
	case <-time.After(100 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("concurrent extraction queued instead of rejecting")
	}
	cancel()
	<-done
}
