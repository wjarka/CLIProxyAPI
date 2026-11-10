package auth

import (
	"context"
	"math"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestManagerPublishesConsoleRoutingSnapshot(t *testing.T) {
	if _, ok := reflect.TypeOf((*Manager)(nil)).MethodByName("SetConsoleRoutingSnapshot"); !ok {
		t.Fatal("manager cannot publish cached Console routing observations")
	}
}

func TestConsoleGrantRankingAndAffinityRebind(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	a2 := &Auth{ID: "a2", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a2"}}
	b := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
	grant := func(days int, amount float64) ConsoleRoutingGrant {
		return ConsoleRoutingGrant{Remaining: amount, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
	}
	obs := func(a *Auth, org string, grants ...ConsoleRoutingGrant) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "claude", AccountID: org, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(5 * time.Minute), Grants: grants}
	}
	snapshot := ConsoleRoutingSnapshot{"a": obs(a, "claude:apptension", grant(1, 1), grant(31, 1)), "a2": obs(a2, "claude:apptension", grant(1, 1), grant(31, 1)), "b": obs(b, "claude:wiktor", grant(5, 1))}
	m := &Manager{}
	m.SetConsoleRoutingSnapshot(snapshot)
	ctx := m.consoleRoutingContext(context.Background())
	s := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer s.Stop()
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"thread-1"}}}
	auths := []*Auth{a, a2, b}
	pick := func(ctx context.Context, want string) {
		t.Helper()
		got, err := s.Pick(ctx, "claude", "model", opts, auths)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v, %v; want %s", got, err, want)
		}
	}
	pick(ctx, "a")
	// A published snapshot must not change when its input is later modified.
	for _, id := range []string{"a", "a2"} {
		o := snapshot[id]
		o.Grants[0].Remaining = 0
		snapshot[id] = o
	}
	pick(ctx, "a")
	m.SetConsoleRoutingSnapshot(snapshot)
	ctx = m.consoleRoutingContext(context.Background())
	pick(ctx, "b")
	// An old success cannot restore the previous credential.
	s.OnResult(Result{AuthID: "a", Provider: "claude", Model: "model", Options: opts, Success: true})
	pick(ctx, "b")
}

func TestConsoleObservationValidation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	a := &Auth{ID: "a", Provider: "claude", Attributes: map[string]string{"api_key": "secret"}}
	o := ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:org", CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{RemainingKnown: true, Remaining: 1, ExpiresAt: now.Add(time.Hour)}}}
	ctx := context.WithValue(context.Background(), consoleRoutingContextKey{}, ConsoleRoutingSnapshot{"a": o})
	if _, ok := consoleObservation(ctx, a); !ok {
		t.Fatal("verified credential missing")
	}
	a.Attributes["header:OpenAI-Project"] = "changed"
	if _, ok := consoleObservation(ctx, a); ok {
		t.Fatal("changed credential retained identity")
	}
	delete(a.Attributes, "header:OpenAI-Project")
	for _, tc := range []struct {
		name   string
		change func(*ConsoleRoutingObservation)
	}{
		{"stale", func(o *ConsoleRoutingObservation) { o.ObservedAt = now.Add(-6 * time.Minute) }}, {"expired validity", func(o *ConsoleRoutingObservation) { o.ValidUntil = now }}, {"identity only", func(o *ConsoleRoutingObservation) { o.ValidUntil = time.Time{} }}, {"unknown", func(o *ConsoleRoutingObservation) { o.Grants[0].RemainingKnown = false }}, {"zero", func(o *ConsoleRoutingObservation) { o.Grants[0].Remaining = 0 }}, {"nan", func(o *ConsoleRoutingObservation) { o.Grants[0].Remaining = math.NaN() }}, {"expired grant", func(o *ConsoleRoutingObservation) { o.Grants[0].ExpiresAt = now }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyO := o
			copyO.Grants = append([]ConsoleRoutingGrant(nil), o.Grants...)
			tc.change(&copyO)
			if got := consoleGrantReset(copyO, now); !got.IsZero() {
				t.Fatalf("unexpected reset %v", got)
			}
		})
	}
}

func TestConsoleGrantLCPRebindPreservesSessionAndRejectsLateSuccess(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	b := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
	mk := func(a *Auth, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	m := &Manager{}
	snap := ConsoleRoutingSnapshot{"a": mk(a, 1), "b": mk(b, 5)}
	m.SetConsoleRoutingSnapshot(snap)
	s := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer s.Stop()
	auths := []*Auth{a, b}
	options := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"system","content":"you help"},{"role":"user","content":"hello"}]}`), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "test"}}
	}
	pick := func(opts cliproxyexecutor.Options, want string) {
		t.Helper()
		got, err := s.Pick(m.consoleRoutingContext(context.Background()), "claude", "model", opts, auths)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v %v, want %s", got, err, want)
		}
	}
	old := options()
	pick(old, "a")
	oldID := old.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey]
	if oldID == nil {
		t.Fatal("missing LCP session")
	}
	snap["a"] = mk(a, 31)
	m.SetConsoleRoutingSnapshot(snap)
	next := options()
	pick(next, "b")
	if next.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey] != oldID {
		t.Fatal("rebind changed LCP session")
	}
	s.OnResult(Result{AuthID: "a", Provider: "claude", Model: "model", Options: old, Success: true})
	pick(options(), "b")
}

func TestManagerConsoleSnapshotFeedsSingleSelection(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "console-a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	b := &Auth{ID: "console-b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
	s := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer s.Stop()
	m := NewManager(nil, s, nil)
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude"})
	for _, a := range []*Auth{a, b} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	obs := func(a *Auth, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	snapshot := ConsoleRoutingSnapshot{a.ID: obs(a, 1), b.ID: obs(b, 5)}
	m.SetConsoleRoutingSnapshot(snapshot)
	for _, provider := range []string{"claude"} {
		opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"manager-test"}}}
		got, err := m.SelectAuth(context.Background(), provider, "", opts)
		if err != nil || got.ID != a.ID {
			t.Fatalf("first selection: %v %v", got, err)
		}
		snapshot[a.ID] = obs(a, 31)
		m.SetConsoleRoutingSnapshot(snapshot)
		got, err = m.SelectAuth(context.Background(), provider, "", opts)
		if err != nil || got.ID != b.ID {
			t.Fatalf("reselection: %v %v", got, err)
		}
	}
}

func TestConsoleRoutingNilContext(t *testing.T) {
	m := &Manager{}
	m.SetConsoleRoutingSnapshot(nil)
	if m.consoleRoutingContext(nil) == nil {
		t.Fatal("nil context")
	}
	if _, ok := consoleObservation(nil, &Auth{}); ok {
		t.Fatal("nil context supplied observation")
	}
}

func TestManagerConsoleSnapshotFeedsMixedSelection(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "mixed-a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	b := &Auth{ID: "mixed-b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
	s := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer s.Stop()
	m := NewManager(nil, s, nil)
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude"})
	for _, a := range []*Auth{a, b} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	obs := func(a *Auth, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	snap := ConsoleRoutingSnapshot{a.ID: obs(a, 1), b.ID: obs(b, 5)}
	m.SetConsoleRoutingSnapshot(snap)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"mixed-session"}}}
	pick := func(want string) {
		t.Helper()
		got, _, _, err := m.pickNextMixedLegacy(context.Background(), []string{"claude"}, "", opts, nil)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v %v; want %s", got, err, want)
		}
	}
	pick(a.ID)
	snap[a.ID] = obs(a, 31)
	m.SetConsoleRoutingSnapshot(snap)
	pick(b.ID)
}

func TestConsoleAffinityCohortsAndOtherStrategies(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	b := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
	mk := func(a *Auth, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	m := &Manager{}
	snapshot := ConsoleRoutingSnapshot{"a": mk(a, 1), "b": mk(b, 2)}
	m.SetConsoleRoutingSnapshot(snapshot)
	for name, fallback := range map[string]Selector{"earliest": &EarliestResetSelector{}, "round-robin": &RoundRobinSelector{}, "fill-first": &FillFirstSelector{}} {
		t.Run(name, func(t *testing.T) {
			s := NewSessionAffinitySelector(fallback)
			defer s.Stop()
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"cohort-" + name}}}
			auths := []*Auth{a, b}
			ctx := m.consoleRoutingContext(context.Background())
			got, err := s.Pick(ctx, "claude", "", opts, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("initial %v %v", got, err)
			}
			snapshot["a"] = mk(a, 2)
			m.SetConsoleRoutingSnapshot(snapshot)
			got, err = s.Pick(m.consoleRoutingContext(context.Background()), "claude", "", opts, auths)
			if err != nil || got.ID != "a" {
				t.Fatalf("tie changed binding: %v %v", got, err)
			}
			snapshot["a"] = mk(a, 3)
			m.SetConsoleRoutingSnapshot(snapshot)
			got, err = s.Pick(m.consoleRoutingContext(context.Background()), "claude", "", opts, auths)
			want := "a"
			if name == "earliest" {
				want = "b"
			}
			if err != nil || got.ID != want {
				t.Fatalf("strategy: %v %v want %s", got, err, want)
			}
			snapshot["a"] = mk(a, 1)
			m.SetConsoleRoutingSnapshot(snapshot)
		})
	}
}

func TestConsoleRoutingConcurrentPublishAndRead(t *testing.T) {
	m := &Manager{}
	a := &Auth{ID: "a", Provider: "claude", Attributes: map[string]string{"api_key": "a"}}
	o := ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:a", CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: time.Now(), ValidUntil: time.Now().Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: time.Now().Add(time.Hour)}}}
	snapshot := ConsoleRoutingSnapshot{"a": o}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				m.SetConsoleRoutingSnapshot(snapshot)
				ctx := m.consoleRoutingContext(context.Background())
				consoleObservation(ctx, a)
			}
		}()
	}
	wg.Wait()
}

func TestConsoleLCPLateExtensionDoesNotRestoreOldOrganization(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	b := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
	obs := func(a *Auth, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	m := &Manager{}
	snapshot := ConsoleRoutingSnapshot{"a": obs(a, 1), "b": obs(b, 5)}
	m.SetConsoleRoutingSnapshot(snapshot)
	s := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer s.Stop()
	options := func(grown bool) cliproxyexecutor.Options {
		payload := `{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"}]}`
		if grown {
			payload = `{"messages":[{"role":"system","content":"stable"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"continue"}]}`
		}
		return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(payload), Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "late-extension"}}
	}
	pick := func(ctx context.Context, opts cliproxyexecutor.Options, want string) {
		t.Helper()
		got, err := s.Pick(ctx, "claude", "model", opts, []*Auth{a, b})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v %v, want %s", got, err, want)
		}
	}
	oldCtx := m.consoleRoutingContext(context.Background())
	root := options(false)
	pick(oldCtx, root, "a")
	oldExtension := options(true)
	pick(oldCtx, oldExtension, "a")
	snapshot["a"] = obs(a, 31)
	m.SetConsoleRoutingSnapshot(snapshot)
	current := m.consoleRoutingContext(context.Background())
	pick(current, options(false), "b")
	s.OnResult(Result{AuthID: "a", Provider: "claude", Model: "model", Options: oldExtension, Success: true})
	// Remove ranking to observe the actual cached identity rather than masking an old binding with re-ranking.
	pick(context.Background(), options(true), "b")
}

func TestConsoleAffinityParentAliasForkAndSubagent(t *testing.T) {
	cases := []struct {
		name           string
		parent, child  cliproxyexecutor.Options
		parentRetained bool
	}{
		{"alias", cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":{"id":"parent-alias"}}`), Metadata: map[string]any{}}, cliproxyexecutor.Options{OriginalRequest: []byte(`{"conversation":{"id":"parent-alias"},"prompt_cache_key":"new-key"}`), Metadata: map[string]any{}}, false},
		{"fork", cliproxyexecutor.Options{OriginalRequest: []byte(`{"sessionId":"parent-fork"}`), Metadata: map[string]any{}}, cliproxyexecutor.Options{OriginalRequest: []byte(`{"sessionId":"child-fork","forkSource":{"sessionId":"parent-fork"}}`), Metadata: map[string]any{}}, true},
		{"subagent", cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"parent-sub"}}, Metadata: map[string]any{}}, cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"parent-sub"}, "X-Claude-Code-Agent-Id": []string{"agent-1"}}, Metadata: map[string]any{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			a := &Auth{ID: "a", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
			b := &Auth{ID: "b", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "b"}}
			obs := func(a *Auth, days int) ConsoleRoutingObservation {
				return ConsoleRoutingObservation{Provider: "claude", AccountID: "claude:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "claude"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
			}
			m := &Manager{}
			snapshot := ConsoleRoutingSnapshot{"a": obs(a, 1), "b": obs(b, 5)}
			m.SetConsoleRoutingSnapshot(snapshot)
			s := NewSessionAffinitySelector(&EarliestResetSelector{})
			defer s.Stop()
			pick := func(opts cliproxyexecutor.Options, want string) {
				t.Helper()
				got, err := s.Pick(m.consoleRoutingContext(context.Background()), "claude", "model", opts, []*Auth{a, b})
				if err != nil || got == nil || got.ID != want {
					t.Fatalf("got %v %v, want %s", got, err, want)
				}
			}
			pick(tc.parent, "a")
			snapshot["a"] = obs(a, 31)
			m.SetConsoleRoutingSnapshot(snapshot)
			pick(tc.child, "b")
			parentID, _ := extractExplicitSessionIDs(tc.parent.Headers, tc.parent.OriginalRequest, tc.parent.Metadata)
			parentKey := "claude::" + parentID + "::model"
			got, ok := s.cache.Get(parentKey)
			want := "b"
			if tc.parentRetained {
				want = "a"
			}
			if !ok || got != want {
				t.Fatalf("parent cache = %q %v, want %s", got, ok, want)
			}
		})
	}
}

func TestConsoleAffinityPriorityCooldownAndWebsocket(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "a", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "a", "base_url": "https://api.openai.com/v1", "priority": "2"}}
	b := &Auth{ID: "b", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "b", "base_url": "https://api.openai.com/v1", "priority": "1", "websockets": "true"}}
	obs := func(a *Auth, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: "openai", AccountID: "openai:" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, "openai"), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	m := &Manager{}
	m.SetConsoleRoutingSnapshot(ConsoleRoutingSnapshot{"a": obs(a, 1), "b": obs(b, 5)})
	ctx := m.consoleRoutingContext(context.Background())
	s := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer s.Stop()
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"priority-recovery"}}, Metadata: map[string]any{}}
	auths := []*Auth{a, b}
	pick := func(ctx context.Context, want string) {
		t.Helper()
		got, err := s.Pick(ctx, "codex", "model", opts, auths)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v %v, want %s", got, err, want)
		}
	}
	a.ModelStates = map[string]*ModelState{"model": {Status: StatusActive, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true}}}
	pick(ctx, "b")
	delete(a.ModelStates, "model")
	pick(ctx, "a")
	b.Attributes["priority"] = "2"
	pick(cliproxyexecutor.WithDownstreamWebsocket(ctx), "b")
}

func TestConsoleMixedSelectionHonorsModelEligibility(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "console-model-claude", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "a"}}
	b := &Auth{ID: "console-model-openai", Provider: "openai", Status: StatusActive, Attributes: map[string]string{"api_key": "b", "base_url": "https://api.openai.com/v1"}}
	obs := func(a *Auth, provider string, days int) ConsoleRoutingObservation {
		return ConsoleRoutingObservation{Provider: provider, AccountID: provider + ":" + a.ID, CredentialFingerprint: BillingCredentialFingerprint(a, provider), ObservedAt: now, ValidUntil: now.Add(time.Minute), Grants: []ConsoleRoutingGrant{{Remaining: 1, RemainingKnown: true, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}}}
	}
	selector := NewSessionAffinitySelector(&EarliestResetSelector{})
	defer selector.Stop()
	m := NewManager(nil, selector, nil)
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "claude"})
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "openai"})
	for _, a := range []*Auth{a, b} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "other-model"}})
	registry.GetGlobalRegistry().RegisterClient(b.ID, "openai", []*registry.ModelInfo{{ID: "console-mixed-model"}})
	m.SetConsoleRoutingSnapshot(ConsoleRoutingSnapshot{a.ID: obs(a, "claude", 1), b.ID: obs(b, "openai", 5)})
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"mixed-model-session"}}, Metadata: map[string]any{}}
	pick := func(want string) {
		t.Helper()
		got, _, _, err := m.pickNextMixedLegacy(context.Background(), []string{"claude", "openai"}, "console-mixed-model", opts, nil)
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("got %v %v want %s", got, err, want)
		}
	}
	pick(b.ID)
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "console-mixed-model"}})
	pick(a.ID)
}
