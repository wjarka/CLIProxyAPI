package consolebilling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMissingBillingSchemaNeverAuthenticates(t *testing.T) {
	for _, p := range []Provider{Claude, OpenAI} {
		t.Run(string(p), func(t *testing.T) {
			m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
				if r.URL.Path == "/api/organizations" {
					return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
				}
				if r.URL.Path == "/v1/me" {
					return response(200, `{"orgs":{"data":[{"id":"org-one"}]}}`)
				}
				return response(200, `{}`)
			}))
			if _, e := m.Connect(context.Background(), p, "x", "fixture-session"); e == nil {
				t.Fatal("empty response authenticated connection with invented unknown balance")
			}
		})
	}
}
func TestRefreshPreservesBalanceAndRequiresReconnect(t *testing.T) {
	var expired atomic.Bool
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if expired.Load() {
			return response(401, `{"message":"private-secret"}`)
		}
		if r.URL.Path == "/api/organizations" {
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, e := m.Connect(context.Background(), Claude, "primary", "private-secret")
	if e != nil {
		t.Fatal(e)
	}
	before := m.List().Accounts[0]
	expired.Store(true)
	if e = m.Refresh(context.Background(), c.ID); e == nil {
		t.Fatal("expired refresh succeeded")
	}
	s := m.List()
	a := s.Accounts[0]
	if a.Balance == nil || *a.Balance != 5 || a.Status != "stale" || !reflect.DeepEqual(a.ObservedAt, before.ObservedAt) || s.Connections[0].Status != "reconnect_required" {
		t.Fatalf("failed refresh replaced billing data %#v", s)
	}
	if strings.Contains(a.Error, "private-secret") {
		t.Fatal("upstream error leaked")
	}
}
func TestReconnectRetainsBindingsAndRejectsDifferentOrganization(t *testing.T) {
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			org := "org-one"
			if strings.Contains(r.Header.Get("Cookie"), "other") {
				org = "org-other"
			}
			return response(200, `[{"uuid":"`+org+`","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, e := m.Connect(context.Background(), Claude, "primary", "first")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Bind("key", c.AccountIDs[0], true); e != nil {
		t.Fatal(e)
	}
	before := m.List()
	if _, e = m.Reconnect(context.Background(), c.ID, "other"); e == nil {
		t.Fatal("organization swap succeeded")
	}
	if !reflect.DeepEqual(m.List(), before) {
		t.Fatal("mismatch changed existing account")
	}
	if e = m.DeleteConnection(c.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Reconnect(context.Background(), c.ID, "replacement"); e != nil {
		t.Fatal(e)
	}
	s := m.List()
	if s.Connections[0].ID != c.ID || s.Accounts[0].ID != c.AccountIDs[0] || !s.Bindings[0].Verified {
		t.Fatal("reconnect lost identity or binding")
	}
}
func TestRefreshCannotResurrectDeletedSession(t *testing.T) {
	var block atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			if block.Load() {
				close(entered)
				<-release
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, e := m.Connect(context.Background(), Claude, "primary", "first")
	if e != nil {
		t.Fatal(e)
	}
	block.Store(true)
	done := make(chan error, 1)
	go func() { done <- m.Refresh(context.Background(), c.ID) }()
	<-entered
	if e = m.DeleteConnection(c.ID); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e = <-done; e == nil || e.Error() != "superseded" {
		t.Fatalf("stale refresh result %v", e)
	}
	if m.List().Connections[0].Status != "reconnect_required" || m.List().Accounts[0].Status != "stale" {
		t.Fatal("stale refresh resurrected connection")
	}
	if e = m.Refresh(context.Background(), c.ID); e == nil {
		t.Fatal("deleted session usable")
	}
}
func TestRefreshCannotOverwriteReconnect(t *testing.T) {
	var block atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		old := strings.Contains(r.Header.Get("Cookie"), "first")
		if r.URL.Path == "/api/organizations" {
			if block.Load() && old {
				close(entered)
				<-release
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		if old {
			return response(200, `{"amount":500}`)
		}
		return response(200, `{"amount":900}`)
	}))
	c, e := m.Connect(context.Background(), Claude, "primary", "first")
	if e != nil {
		t.Fatal(e)
	}
	block.Store(true)
	done := make(chan error, 1)
	go func() { done <- m.Refresh(context.Background(), c.ID) }()
	<-entered
	if _, e = m.Reconnect(context.Background(), c.ID, "replacement"); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e = <-done; e == nil || e.Error() != "superseded" {
		t.Fatalf("stale refresh result %v", e)
	}
	if *m.List().Accounts[0].Balance != 9 {
		t.Fatal("old session overwrote reconnected billing")
	}
}
func TestCorruptionFailsClosedAndMissingKeyIsNotRegenerated(t *testing.T) {
	for _, missingKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "ciphertext", true: "missing-key"}[missingKey], func(t *testing.T) {
			dir := t.TempDir()
			m, e := NewManager(dir, claudeFixture(t, `{"amount":1}`))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = m.Connect(context.Background(), Claude, "x", "fixture-secret"); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(dir, "billing.enc")
			b, _ := os.ReadFile(path)
			if missingKey {
				if e = os.Remove(filepath.Join(dir, "session.key")); e != nil {
					t.Fatal(e)
				}
			} else {
				b[len(b)-1] ^= 1
				if e = os.WriteFile(path, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = NewManager(dir, nil); e == nil {
				t.Fatal("corruption accepted")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(b) {
				t.Fatal("corrupt ciphertext replaced")
			}
			if missingKey {
				if _, e = os.Stat(filepath.Join(dir, "session.key")); !os.IsNotExist(e) {
					t.Fatal("key regenerated and corrupt data displaced")
				}
			}
		})
	}
}
func TestSnapshotIsDetachedAndArraysNeverNull(t *testing.T) {
	m := newTestManager(t, claudeFixture(t, `{"amount":100,"tranches":[{"remaining_amount_minor_units":80}]}`))
	b, _ := json.Marshal(m.List())
	if string(b) != `{"connections":[],"accounts":[],"bindings":[]}` {
		t.Fatalf("null arrays %s", b)
	}
	c, e := m.Connect(context.Background(), Claude, "x", "fixture-session")
	if e != nil {
		t.Fatal(e)
	}
	s := m.List()
	*s.Accounts[0].Balance = 1000
	*s.Accounts[0].Grants[0].Remaining = 1000
	s.Connections[0].AccountIDs[0] = "bad"
	c.AccountIDs[0] = "also-bad"
	s2 := m.List()
	if *s2.Accounts[0].Balance != 1 || *s2.Accounts[0].Grants[0].Remaining != .8 || s2.Connections[0].AccountIDs[0] == "bad" || s2.Connections[0].AccountIDs[0] == "also-bad" {
		t.Fatal("snapshot aliases mutable manager state")
	}
}

func TestSnapshotAgeMarksStaleWithoutChangingPersistedData(t *testing.T) {
	m := newTestManager(t, claudeFixture(t, `{"amount":500}`))
	if _, e := m.Connect(context.Background(), Claude, "x", "fixture-session"); e != nil {
		t.Fatal(e)
	}
	m.mu.Lock()
	old := time.Now().Add(-10 * time.Minute)
	m.state.Snapshot.Accounts[0].ObservedAt = &old
	m.mu.Unlock()
	s := m.List()
	if s.Accounts[0].Status != "stale" || *s.Accounts[0].Balance != 5 {
		t.Fatal("old observation remains fresh or balance erased")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Snapshot.Accounts[0].Status != "ready" {
		t.Fatal("derived freshness mutated persisted state")
	}
}
func TestCompetingManagerCannotOverwriteNewerState(t *testing.T) {
	dir := t.TempDir()
	c := claudeFixture(t, `{"amount":500}`)
	first, e := NewManager(dir, c)
	if e != nil {
		t.Fatal(e)
	}
	conn, e := first.Connect(context.Background(), Claude, "one", "fixture-one")
	if e != nil {
		t.Fatal(e)
	}
	second, e := NewManager(dir, c)
	if e != nil {
		t.Fatal(e)
	}
	if e = first.Bind("credential-first", conn.AccountIDs[0], false); e != nil {
		t.Fatal(e)
	}
	if e = second.Bind("credential-second", conn.AccountIDs[0], false); e == nil {
		t.Fatal("stale manager overwrote newer state")
	}
	third, e := NewManager(dir, c)
	if e != nil {
		t.Fatal(e)
	}
	s := third.List()
	if len(s.Bindings) != 1 || s.Bindings[0].CredentialID != "credential-first" {
		t.Fatal("newer binding lost")
	}
}

func TestSharedOrganizationOlderRequestCannotOverwriteNewerLogin(t *testing.T) {
	var block atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		old := strings.Contains(r.Header.Get("Cookie"), "first")
		if r.URL.Path == "/api/organizations" {
			if block.Load() && old {
				close(entered)
				<-release
			}
			return response(200, `[{"uuid":"org-shared","capabilities":["api"]}]`)
		}
		if old {
			return response(200, `{"amount":100}`)
		}
		return response(200, `{"amount":900}`)
	}))
	first, e := m.Connect(context.Background(), Claude, "first", "first")
	if e != nil {
		t.Fatal(e)
	}
	block.Store(true)
	done := make(chan error, 1)
	go func() { done <- m.Refresh(context.Background(), first.ID) }()
	<-entered
	second, e := m.Connect(context.Background(), Claude, "second", "second")
	if e != nil {
		t.Fatal(e)
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	a := m.List().Accounts[0]
	if *a.Balance != 9 || a.ConnectionID != second.ID {
		t.Fatal("older shared-org response overwrote newer login")
	}
}
func TestPartialOrganizationAccessPreservesExistingData(t *testing.T) {
	var denied atomic.Bool
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			return response(200, `[{"uuid":"org-one","capabilities":["api"]},{"uuid":"org-two","capabilities":["api"]}]`)
		}
		if strings.Contains(r.URL.Path, "org-two") && denied.Load() {
			return response(403, `{"error":"private-fixture"}`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, e := m.Connect(context.Background(), Claude, "primary", "first")
	if e != nil {
		t.Fatal(e)
	}
	denied.Store(true)
	if e = m.Refresh(context.Background(), c.ID); e != nil {
		t.Fatalf("partial refresh discarded readable account: %v", e)
	}
	s := m.List()
	if len(s.Accounts) != 2 || s.Accounts[0].Status != "ready" || s.Accounts[1].Status != "stale" || *s.Accounts[1].Balance != 5 || s.Accounts[1].Error != "permission_denied" {
		t.Fatalf("partial refresh lost prior denied data %#v", s)
	}
	fresh := newTestManager(t, m.client)
	if _, e = fresh.Connect(context.Background(), Claude, "partial", "first"); e != nil {
		t.Fatal(e)
	}
	a := fresh.List().Accounts[1]
	if a.Status != "unknown" || a.Balance != nil || a.ObservedAt != nil || a.Error != "permission_denied" {
		t.Fatalf("inaccessible account fabricated data %#v", a)
	}
}

func TestVerifiedFingerprintPersistsAndManualBindingClearsIt(t *testing.T) {
	dir := t.TempDir()
	client := claudeFixture(t, `{"amount":500}`)
	m, e := NewManager(dir, client)
	if e != nil {
		t.Fatal(e)
	}
	c, e := m.Connect(context.Background(), Claude, "one", "fixture-session")
	if e != nil {
		t.Fatal(e)
	}
	fingerprint := strings.Repeat("a", 64)
	if e = m.BindVerified("credential", c.AccountIDs[0], fingerprint); e != nil {
		t.Fatal(e)
	}
	restored, e := NewManager(dir, client)
	if e != nil {
		t.Fatal(e)
	}
	b := restored.List().Bindings[0]
	if !b.Verified || b.VerificationFingerprint != fingerprint {
		t.Fatal("verification evidence lost across restart")
	}
	if e = restored.Bind("credential", c.AccountIDs[0], false); e != nil {
		t.Fatal(e)
	}
	b = restored.List().Bindings[0]
	if b.Verified || b.VerificationFingerprint != "" {
		t.Fatal("manual rebind retained obsolete verification")
	}
}

func TestCanceledDiscoveryCannotPublishConnectOrReconnect(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(reconnect), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var cancelDiscovery bool
			m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
				if r.URL.Path == "/api/organizations" {
					return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
				}
				if cancelDiscovery {
					cancel()
				}
				return response(200, `{"amount":500}`)
			}))
			var id string
			if reconnect {
				c, err := m.Connect(context.Background(), Claude, "existing", "old")
				if err != nil {
					t.Fatal(err)
				}
				id = c.ID
				if err = m.Bind("key", c.AccountIDs[0], true); err != nil {
					t.Fatal(err)
				}
			}
			before := m.List()
			cancelDiscovery = true
			var err error
			if reconnect {
				_, err = m.Reconnect(ctx, id, "replacement")
			} else {
				_, err = m.Connect(ctx, Claude, "new", "replacement")
			}
			if err == nil || !reflect.DeepEqual(before, m.List()) {
				t.Fatal("canceled discovery published connection state")
			}
		})
	}
}
