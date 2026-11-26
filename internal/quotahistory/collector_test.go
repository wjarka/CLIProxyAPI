package quotahistory

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	accounts []Account
	fetch    func(context.Context, Account) ([]PoolObservation, error)
}

func (f *fakeSource) Accounts(context.Context) ([]Account, error) { return f.accounts, nil }
func (f *fakeSource) Fetch(ctx context.Context, a Account) ([]PoolObservation, error) {
	return f.fetch(ctx, a)
}

func TestCollectorScheduleAndSharedRefresh(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), Retention)
	s.now = func() time.Time { return now }
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	src := &fakeSource{accounts: []Account{{ID: "a", Provider: "claude", CredentialIDs: []string{"credential"}}}}
	src.fetch = func(ctx context.Context, a Account) ([]PoolObservation, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		o := testObservation(now, number(23))
		o.AccountID = a.ID
		return []PoolObservation{o}, nil
	}
	c := NewCollector(s, src, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Tick(ctx)
	<-entered
	_ = c.Refresh(ctx, []string{"credential"})
	close(release)
	c.Wait()
	mu.Lock()
	if calls != 1 {
		t.Fatal(calls)
	}
	mu.Unlock()
	if !c.next["a"].Equal(now.Add(time.Minute)) {
		t.Fatalf("near reset: %v", c.next["a"])
	}
	c.Tick(ctx)
	c.Wait()
	_ = c.Refresh(ctx, []string{"credential"})
	c.Wait()
	mu.Lock()
	if calls != 1 {
		t.Fatal("re-polled early")
	}
	mu.Unlock()
	if len(c.Snapshot(now.Add(-time.Hour), now.Add(time.Hour)).Cycles) != 1 {
		t.Fatal("missing observation")
	}
}

func TestCollectorNormalCadence(t *testing.T) {
	now := time.Now()
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), Retention)
	src := &fakeSource{accounts: []Account{{ID: "a", Provider: "claude"}}, fetch: func(context.Context, Account) ([]PoolObservation, error) { return nil, nil }}
	c := NewCollector(s, src, func() time.Time { return now })
	c.Tick(context.Background())
	c.Wait()
	if delay := c.next["a"].Sub(now); delay < 15*time.Minute || delay > 15*time.Minute+30*time.Second {
		t.Fatal(delay)
	}
}

func TestCollectorEntersFinalTenMinutesBeforeNextNormalPoll(t *testing.T) {
	now := time.Now()
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), Retention)
	src := &fakeSource{accounts: []Account{{ID: "a", Provider: "claude"}}, fetch: func(context.Context, Account) ([]PoolObservation, error) {
		o := testObservation(now, number(23))
		o.AccountID = "a"
		o.ResetAt = now.Add(12 * time.Minute)
		return []PoolObservation{o}, nil
	}}
	c := NewCollector(s, src, func() time.Time { return now })
	c.Tick(context.Background())
	c.Wait()
	if !c.next["a"].Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("missed final sampling window: %v", c.next["a"].Sub(now))
	}
}

func TestCollectorBoundsConcurrencyAndMakesProgress(t *testing.T) {
	now := time.Now()
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), Retention)
	entered := make(chan string, 4)
	release := make(chan struct{})
	src := &fakeSource{accounts: []Account{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}, fetch: func(ctx context.Context, a Account) ([]PoolObservation, error) {
		entered <- a.ID
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}}
	c := NewCollector(s, src, func() time.Time { return now })
	c.Tick(context.Background())
	for i := 0; i < 3; i++ {
		<-entered
	}
	select {
	case <-entered:
		t.Fatal("exceeded three workers")
	default:
	}
	close(release)
	c.Wait()
	c.Tick(context.Background())
	c.Wait()
	if got := <-entered; got != "d" {
		t.Fatal("starved queued account", got)
	}
}

func TestCollectorHonorsRetryAfterAndDisables(t *testing.T) {
	now := time.Now()
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), Retention)
	calls := 0
	src := &fakeSource{accounts: []Account{{ID: "a", Provider: "codex"}}, fetch: func(context.Context, Account) ([]PoolObservation, error) {
		calls++
		o := testObservation(now, number(23))
		o.AccountID = "a"
		o.Provider = "codex"
		return []PoolObservation{o}, &FetchError{Code: "rate_limited", RetryAt: now.Add(time.Hour)}
	}}
	c := NewCollector(s, src, func() time.Time { return now })
	c.Tick(context.Background())
	c.Wait()
	_ = c.Refresh(context.Background(), nil)
	c.Wait()
	if calls != 1 {
		t.Fatal("bypassed provider backoff")
	}
	if len(c.Snapshot(now.Add(-time.Hour), now.Add(time.Hour)).Cycles) != 1 {
		t.Fatal("discarded partial observations")
	}
	status := c.Snapshot(now, now.Add(time.Hour)).Collection
	if len(status) != 1 || status[0].ErrorCode != "rate_limited" {
		t.Fatal(status)
	}
	src.accounts[0].Disabled = true
	now = now.Add(2 * time.Hour)
	c.Tick(context.Background())
	c.Wait()
	if calls != 1 {
		t.Fatal("disabled collected")
	}
}
