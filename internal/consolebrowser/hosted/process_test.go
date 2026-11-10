package hosted

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCDPDiscoveryRejectsForeignSocket(t *testing.T) {
	for _, u := range []string{"ws://evil.test:9222/devtools/page/a", "ws://127.0.0.1:9223/devtools/page/a", "ws://user@127.0.0.1:9222/devtools/page/a", "ws://127.0.0.1:9222/devtools/page/a?token=secret", "ws://127.0.0.1:9222/not-devtools"} {
		if validCDPURL(u, 9222) {
			t.Fatal(u)
		}
	}
	if !validCDPURL("ws://127.0.0.1:9222/devtools/page/ABC-123", 9222) {
		t.Fatal("rejected local socket")
	}
}
func TestProcessStartupFailureRemovesProfile(t *testing.T) {
	root := t.TempDir()
	c, e := NewProcessController(root)
	if e != nil {
		t.Fatal(e)
	}
	c.xvfb = filepath.Join(root, "missing-Xvfb")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, e = c.Start(ctx, "claude"); e == nil {
		t.Fatal("missing process succeeded")
	}
	entries, e := os.ReadDir(root)
	if e != nil || len(entries) != 0 {
		t.Fatal("profile leaked", entries, e)
	}
}
func TestProcessExitCleansProfile(t *testing.T) {
	root := t.TempDir()
	c, e := NewProcessController(root)
	if e != nil {
		t.Fatal(e)
	}
	p := &processInstance{root: filepath.Join(root, "profile"), done: make(chan struct{}), release: func() {}}
	if e = os.Mkdir(p.root, 0700); e != nil {
		t.Fatal(e)
	}
	if e = p.start(exec.Command("sh", "-c", "exit 0")); e != nil {
		t.Fatal(e)
	}
	go p.monitor()
	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("crash not observed")
	}
	if _, e = os.Stat(p.root); !os.IsNotExist(e) {
		t.Fatal("profile not removed", e)
	}
	_ = c
}
func TestDiscoveryRejectsRedirectWithoutFollowing(t *testing.T) {
	hit := false
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer foreign.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, 302) }))
	defer s.Close()
	_, _ = discoverPage(context.Background(), s.URL, "claude", 9222)
	if hit {
		t.Fatal("followed foreign redirect")
	}
}
