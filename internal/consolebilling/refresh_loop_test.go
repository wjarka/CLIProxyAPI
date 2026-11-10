package consolebilling

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChangesOnlyAfterCommittedSaveAndCoalesces(t *testing.T) {
	dir := t.TempDir()
	client := claudeFixture(t, `{"amount":500}`)
	m, err := NewManager(dir, client)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Connect(context.Background(), Claude, "x", "session")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewManager(dir, client)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Bind("one", c.AccountIDs[0], true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Changes():
	default:
		t.Fatal("missing committed notification")
	}
	select {
	case <-m.Changes():
		t.Fatal("notifications did not coalesce")
	default:
	}
	if err = other.Bind("two", c.AccountIDs[0], true); err == nil {
		t.Fatal("expected storage conflict")
	}
	select {
	case <-other.Changes():
		t.Fatal("failed save notified")
	default:
	}
}

func TestCanceledRefreshDoesNotPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelOnBilling := false
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		if cancelOnBilling {
			cancel()
		}
		return response(200, `{"amount":500}`)
	}))
	c, err := m.Connect(context.Background(), Claude, "x", "session")
	if err != nil {
		t.Fatal(err)
	}
	before := m.List()
	cancelOnBilling = true
	if err = m.Refresh(ctx, c.ID); err == nil {
		t.Fatal("canceled refresh succeeded")
	}
	if !reflect.DeepEqual(before, m.List()) {
		t.Fatal("canceled refresh published")
	}
}

func TestRefreshLoopInitialPeriodicAndShutdown(t *testing.T) {
	var calls atomic.Int32
	observed := make(chan struct{}, 10)
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			calls.Add(1)
			observed <- struct{}{}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, err := m.Connect(context.Background(), Claude, "x", "session")
	if err != nil {
		t.Fatal(err)
	}
	<-observed
	<-m.Changes()
	ticks := make(chan time.Time)
	var clock atomic.Int64
	clock.Store(1000)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runRefreshLoop(ctx, time.Minute, ticks, now); close(done) }()
	<-observed
	// Wait until the loop has recorded the result before advancing the clock.
	<-m.Changes()
	ticks <- now()
	clock.Add(60)
	tickUntilChange(t, ticks, now, m.Changes())
	<-observed
	cancel()
	<-done
	if calls.Load() != 3 {
		t.Fatalf("got %d discoveries", calls.Load())
	}
	if err = m.DeleteConnection(c.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshLoopBackoffAndReconnectRequiredSkip(t *testing.T) {
	var failing atomic.Bool
	var calls atomic.Int32
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			calls.Add(1)
			if failing.Load() {
				return response(503, `{}`)
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, err := m.Connect(context.Background(), Claude, "x", "session")
	if err != nil {
		t.Fatal(err)
	}
	<-m.Changes()
	failing.Store(true)
	var clock atomic.Int64
	clock.Store(1000)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runRefreshLoop(ctx, time.Minute, ticks, now); close(done) }()
	<-m.Changes()  // first failure
	ticks <- now() // result has been scheduled
	clock.Store(1060)
	tickUntilChange(t, ticks, now, m.Changes()) // second failure
	ticks <- now()                              // result has been scheduled
	clock.Store(1120)
	ticks <- now()
	ticks <- now() // second tick confirms the previous sweep finished
	if got := calls.Load(); got != 3 {
		t.Fatalf("retried before exponential backoff elapsed: %d", got)
	}
	clock.Store(1180)
	tickUntilChange(t, ticks, now, m.Changes())
	if got := calls.Load(); got != 4 {
		t.Fatalf("retry after backoff: %d", got)
	}
	if err = m.DeleteConnection(c.ID); err != nil {
		t.Fatal(err)
	}
	clock.Store(1240)
	ticks <- now()
	ticks <- now()
	if got := calls.Load(); got != 4 {
		t.Fatalf("deleted session refreshed: %d", got)
	}
	cancel()
	<-done
}

func TestBackgroundCannotSupersedeActiveManualReconnect(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var block atomic.Bool
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			if block.Load() && strings.Contains(r.Header.Get("Cookie"), "replacement") {
				close(entered)
				<-release
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, err := m.Connect(context.Background(), Claude, "x", "old")
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	done := make(chan error, 1)
	go func() { _, err := m.Reconnect(context.Background(), c.ID, "replacement"); done <- err }()
	<-entered
	if err := m.refresh(context.Background(), c.ID, true); !errors.Is(err, errRefreshSkipped) {
		t.Fatalf("background refresh did not coalesce: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("manual reconnect superseded: %v", err)
	}
}

func TestRefreshLoopShutdownDrainsLateTransportWithoutPublication(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var block atomic.Bool
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			if block.Load() {
				close(entered)
				<-release
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":900}`)
	}))
	_, err := m.Connect(context.Background(), Claude, "x", "session")
	if err != nil {
		t.Fatal(err)
	}
	<-m.Changes()
	before := m.List()
	block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.RunRefreshLoop(ctx, time.Minute); close(done) }()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("loop returned with transport still running")
	default:
	}
	close(release)
	<-done
	if !reflect.DeepEqual(before, m.List()) {
		t.Fatal("late transport published after shutdown")
	}
	select {
	case <-m.Changes():
		t.Fatal("late transport notified")
	default:
	}
}

func TestRefreshLoopSlowConnectionDoesNotBlockHealthyConnection(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	healthy := make(chan struct{}, 8)
	var running atomic.Bool
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		slow := strings.Contains(r.Header.Get("Cookie"), "slow")
		if r.URL.Path == "/api/organizations" {
			if running.Load() && slow {
				close(blocked)
				<-release
			}
			org := "healthy"
			if slow {
				org = "slow"
			}
			return response(200, `[{"uuid":"`+org+`","capabilities":["api"]}]`)
		}
		if running.Load() && !slow {
			healthy <- struct{}{}
		}
		return response(200, `{"amount":500}`)
	}))
	for _, session := range []string{"slow", "healthy"} {
		if _, err := m.Connect(context.Background(), Claude, session, session); err != nil {
			t.Fatal(err)
		}
	}
	<-m.Changes()
	running.Store(true)
	var clock atomic.Int64
	clock.Store(1000)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runRefreshLoop(ctx, time.Minute, ticks, now); close(done) }()
	defer func() { cancel(); close(release); <-done }()
	<-blocked
	<-healthy
	<-m.Changes()
	tickDone := make(chan struct{})
	go func() { ticks <- now(); close(tickDone) }()
	select {
	case <-tickDone:
	case <-time.After(time.Second):
		t.Fatal("slow connection blocked the scheduler")
	}
	clock.Store(1060)
	tickUntilChange(t, ticks, now, m.Changes())
	select {
	case <-healthy:
	case <-time.After(time.Second):
		t.Fatal("healthy connection did not refresh while another is blocked")
	}
}

func TestRefreshLoopIntervalMeasuredFromDispatch(t *testing.T) {
	var running atomic.Bool
	var clock atomic.Int64
	clock.Store(1000)
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := make(chan struct{}, 4)
	var attempts atomic.Int32
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			if running.Load() {
				calls <- struct{}{}
				if attempts.Add(1) == 1 {
					close(entered)
					<-release
				}
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	if _, err := m.Connect(context.Background(), Claude, "x", "session"); err != nil {
		t.Fatal(err)
	}
	<-m.Changes()
	running.Store(true)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runRefreshLoop(ctx, time.Minute, ticks, now); close(done) }()
	defer func() { cancel(); <-done }()
	<-entered
	<-calls
	clock.Store(1005)
	close(release)
	<-m.Changes()
	ticks <- now() // let the loop process completion
	clock.Store(1060)
	deadline := time.After(time.Second)
	for {
		select {
		case ticks <- now():
		case <-deadline:
			t.Fatal("refresh missed its next interval")
		}
		select {
		case <-calls:
			return
		default:
		}
	}
}

func TestManualRecoveryClearsBackgroundBackoff(t *testing.T) {
	var clock atomic.Int64
	clock.Store(1000)
	var failing atomic.Bool
	calls := make(chan struct{}, 10)
	m := newTestManager(t, fixtureClient(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/organizations" {
			calls <- struct{}{}
			if failing.Load() {
				return response(503, `{}`)
			}
			return response(200, `[{"uuid":"org-one","capabilities":["api"]}]`)
		}
		return response(200, `{"amount":500}`)
	}))
	c, err := m.Connect(context.Background(), Claude, "x", "session")
	if err != nil {
		t.Fatal(err)
	}
	<-calls
	<-m.Changes()
	failing.Store(true)
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runRefreshLoop(ctx, time.Minute, ticks, now); close(done) }()
	defer func() { cancel(); <-done }()
	<-calls
	<-m.Changes()
	ticks <- now()
	clock.Store(1060)
	deadline := time.After(time.Second)
	for {
		select {
		case ticks <- now():
		case <-deadline:
			t.Fatal("missing second failure")
		}
		select {
		case <-calls:
			goto secondFailure
		default:
		}
	}
secondFailure:
	<-m.Changes()
	failing.Store(false)
	if err := m.Refresh(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	<-calls
	<-m.Changes()
	clock.Store(1120)
	deadline = time.After(time.Second)
	for {
		select {
		case ticks <- now():
		case <-deadline:
			t.Fatal("manual recovery retained backoff")
		}
		select {
		case <-calls:
			return
		default:
		}
	}
}

func tickUntilChange(t *testing.T, ticks chan<- time.Time, now func() time.Time, changes <-chan struct{}) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case <-changes:
			return
		case ticks <- now():
		case <-deadline:
			t.Fatal("refresh did not complete at controlled tick")
		}
	}
}
