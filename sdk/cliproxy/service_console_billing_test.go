package cliproxy

import (
	"context"
	"fmt"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
)

func projectionFixture() consolebilling.Snapshot {
	observed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := observed.Add(time.Hour)
	amount := 12.0
	return consolebilling.Snapshot{
		Connections: []consolebilling.Connection{{ID: "login", Provider: consolebilling.Claude, Status: "ready", AccountIDs: []string{"claude:org"}}},
		Accounts:    []consolebilling.Account{{ID: "claude:org", Provider: consolebilling.Claude, OrganizationID: "org", ConnectionID: "login", Status: "ready", Balance: &amount, ObservedAt: &observed, Grants: []consolebilling.Grant{{ID: "grant", Remaining: &amount, ExpiresAt: &expiry}}}},
		Bindings:    []consolebilling.Binding{{CredentialID: "key", AccountID: "claude:org", Verified: true, VerificationFingerprint: "verified-fingerprint"}},
	}
}

func TestConsoleRoutingProjectionPreservesDetachedGrantObservation(t *testing.T) {
	source := projectionFixture()
	got := consoleRoutingProjection(source)
	if len(got) != 1 {
		t.Fatalf("want one verified credential, got %#v", got)
	}
	item := got["key"]
	if item.Provider != "claude" || item.AccountID != "claude:org" || item.CredentialFingerprint != "verified-fingerprint" {
		t.Fatalf("wrong identity: %#v", item)
	}
	if !item.ValidUntil.Equal(item.ObservedAt.Add(consolebilling.FreshnessWindow)) || len(item.Grants) != 1 || !item.Grants[0].RemainingKnown || item.Grants[0].Remaining != 12 {
		t.Fatalf("wrong observation: %#v", item)
	}
	*source.Accounts[0].Grants[0].Remaining = 0
	*source.Accounts[0].Grants[0].ExpiresAt = source.Accounts[0].ObservedAt.Add(2 * time.Hour)
	if item.Grants[0].Remaining != 12 || !item.Grants[0].ExpiresAt.Equal(item.ObservedAt.Add(time.Hour)) {
		t.Fatal("projection aliases mutable store pointers")
	}
}

func TestConsoleRoutingProjectionAllowsOpenAIGrantsWithoutAggregateBalance(t *testing.T) {
	source := projectionFixture()
	source.Connections[0].Provider = consolebilling.OpenAI
	source.Connections[0].AccountIDs = []string{"openai:org"}
	a := &source.Accounts[0]
	a.Provider = consolebilling.OpenAI
	a.ID = "openai:org"
	a.Balance = nil
	a.Status = "unknown"
	source.Bindings[0].AccountID = a.ID
	item, ok := consoleRoutingProjection(source)["key"]
	if !ok || item.Provider != "openai" || item.ValidUntil.IsZero() || len(item.Grants) != 1 || !item.Grants[0].RemainingKnown {
		t.Fatalf("grants-only OpenAI observation lost: %#v", item)
	}
}

func TestConsoleRoutingProjectionRemovesUnsafePreferenceButRetainsVerifiedIdentity(t *testing.T) {
	cases := map[string]func(*consolebilling.Snapshot){
		"stale":                    func(s *consolebilling.Snapshot) { s.Accounts[0].Status = "stale" },
		"account error":            func(s *consolebilling.Snapshot) { s.Accounts[0].Error = "provider_unavailable" },
		"expired login":            func(s *consolebilling.Snapshot) { s.Connections[0].Status = "reconnect_required" },
		"login error":              func(s *consolebilling.Snapshot) { s.Connections[0].Error = "partial_billing_access" },
		"missing login":            func(s *consolebilling.Snapshot) { s.Connections = nil },
		"foreign login provider":   func(s *consolebilling.Snapshot) { s.Connections[0].Provider = consolebilling.OpenAI },
		"login no longer owns org": func(s *consolebilling.Snapshot) { s.Connections[0].AccountIDs = nil },
		"missing observation":      func(s *consolebilling.Snapshot) { s.Accounts[0].ObservedAt = nil },
		"zero balance":             func(s *consolebilling.Snapshot) { v := 0.0; s.Accounts[0].Balance = &v },
		"invalid balance":          func(s *consolebilling.Snapshot) { v := math.NaN(); s.Accounts[0].Balance = &v },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := projectionFixture()
			change(&s)
			item, ok := consoleRoutingProjection(s)["key"]
			if !ok || item.AccountID != "claude:org" || !item.ValidUntil.IsZero() {
				t.Fatalf("unsafe preference or lost verified identity: %#v", item)
			}
		})
	}
}

func TestConsoleRoutingProjectionRejectsUnverifiedOrMissingBindings(t *testing.T) {
	cases := map[string]func(*consolebilling.Snapshot){
		"unverified":          func(s *consolebilling.Snapshot) { s.Bindings[0].Verified = false },
		"missing fingerprint": func(s *consolebilling.Snapshot) { s.Bindings[0].VerificationFingerprint = "" },
		"missing account":     func(s *consolebilling.Snapshot) { s.Accounts = nil },
		"missing credential":  func(s *consolebilling.Snapshot) { s.Bindings[0].CredentialID = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := projectionFixture()
			change(&s)
			if got := consoleRoutingProjection(s); len(got) != 0 {
				t.Fatalf("unsafe mapping published: %#v", got)
			}
		})
	}
}

func TestConsoleRoutingProjectionDoesNotInventUnknownGrantFields(t *testing.T) {
	s := projectionFixture()
	s.Accounts[0].Grants = []consolebilling.Grant{{ID: "unknown"}, {ID: "invalid", Remaining: new(math.Inf(1))}, {ID: "known", Remaining: new(3.0)}}
	item := consoleRoutingProjection(s)["key"]
	if len(item.Grants) != 3 || item.Grants[0].RemainingKnown || item.Grants[1].RemainingKnown || !item.Grants[2].RemainingKnown || !item.Grants[2].ExpiresAt.IsZero() {
		t.Fatalf("invented grant information: %#v", item.Grants)
	}
}

type consoleProjectionTransport func(*http.Request) (*http.Response, error)

func (f consoleProjectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type consoleProjectionExecutor struct{ serviceTestPluginExecutor }

func (consoleProjectionExecutor) Identifier() string { return "claude" }

func TestServiceConsoleBillingPublishesStartupAndCommittedRefreshWithoutUI(t *testing.T) {
	var exhausted atomic.Bool
	now := time.Now()
	early := now.Add(time.Hour).UTC().Format(time.RFC3339)
	later := now.Add(48 * time.Hour).UTC().Format(time.RFC3339)
	middle := now.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	client := &http.Client{Transport: consoleProjectionTransport(func(r *http.Request) (*http.Response, error) {
		body := `[{"uuid":"a","name":"A","capabilities":["api"]},{"uuid":"b","name":"B","capabilities":["api"]}]`
		if strings.Contains(r.URL.Path, "prepaid/credits") {
			remaining := 100
			expiry := early
			if strings.Contains(r.URL.Path, "/b/") {
				expiry = middle
			}
			if strings.Contains(r.URL.Path, "/a/") && exhausted.Load() {
				remaining = 0
			}
			body = fmt.Sprintf(`{"amount":50000,"currency":"USD","promo_tranches":[{"id":"first","remaining_amount_minor_units":%d,"expires_at":%q},{"id":"later","remaining_amount_minor_units":50000,"expires_at":%q}]}`, remaining, expiry, later)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	billing, err := consolebilling.NewManager(t.TempDir(), client)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := billing.Connect(context.Background(), consolebilling.Claude, "login", "test-session")
	if err != nil {
		t.Fatal(err)
	}
	selector := coreauth.NewSessionAffinitySelector(&coreauth.EarliestResetSelector{})
	defer selector.Stop()
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(consoleProjectionExecutor{})
	for _, id := range []string{"a", "b"} {
		auth := &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": id}}
		if _, err = manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatal(err)
		}
		if err = billing.BindVerified(id, "claude:"+id, coreauth.BillingCredentialFingerprint(auth, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{cfg: &config.Config{}, configPath: filepath.Join(t.TempDir(), "config.yaml"), coreManager: manager, consoleBilling: billing}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := s.startConsoleBilling(ctx)
	if len(options) != 1 {
		t.Fatal("shared store option missing")
	}
	done := s.consoleBillingDone
	s.startConsoleBilling(ctx)
	if s.consoleBillingDone != done {
		t.Fatal("duplicate refresh workers")
	}
	defer func() {
		cancel()
		if err := s.stopConsoleBilling(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	opts := coreexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"same-session"}}}
	pick := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		ticks := time.NewTicker(5 * time.Millisecond)
		defer ticks.Stop()
		for {
			a, e := manager.SelectAuth(context.Background(), "claude", "", opts)
			if e == nil && a.ID == want {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("want %s, got %v, %v", want, a, e)
			case <-ticks.C:
			}
		}
	}
	pick("a")
	exhausted.Store(true)
	if err = billing.Refresh(context.Background(), conn.ID); err != nil {
		t.Fatal(err)
	}
	pick("b")
	// A strategy reload must retain the manager's last committed observation.
	next := coreauth.NewSessionAffinitySelector(&coreauth.EarliestResetSelector{})
	defer next.Stop()
	manager.SetSelector(next)
	pick("b")
	if err = billing.Unbind("b"); err != nil {
		t.Fatal(err)
	}
	// Observe unbind without establishing a new affinity before the asynchronous
	// publisher has processed the committed update.
	manager.SetSelector(&coreauth.EarliestResetSelector{})
	pick("a")
	// Both bindings are removable through the same service-owned store.
	if err = billing.Unbind("a"); err != nil {
		t.Fatal(err)
	}
}

func TestServiceConsoleBillingSkipsHomeAndSurvivesStorageFailure(t *testing.T) {
	s := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	if options := s.startConsoleBilling(context.Background()); len(options) != 0 {
		t.Fatal("blank storage path should not enable polling")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s.configPath = filepath.Join(blocked, "config.yaml")
	if options := s.startConsoleBilling(context.Background()); len(options) != 1 || s.consoleBilling != nil {
		t.Fatal("storage failure must leave ordinary inference available")
	}

	s.cfg.Home.Enabled = true
	if options := s.startConsoleBilling(context.Background()); len(options) != 0 || s.consoleBilling != nil {
		t.Fatal("Home authority must remain external")
	}
}
