package quotahistory

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"time"
)

type Source interface {
	Accounts(context.Context) ([]Account, error)
	Fetch(context.Context, Account) ([]PoolObservation, error)
}

type FetchError struct {
	Code    string
	RetryAt time.Time
}

func (e *FetchError) Error() string { return e.Code }

type Collector struct {
	store    *Store
	source   Source
	now      func() time.Time
	mu       sync.Mutex
	next     map[string]time.Time
	last     map[string]time.Time
	blocked  map[string]time.Time
	failures map[string]int
	running  map[string]bool
	statuses map[string]CollectionStatus
	slots    chan struct{}
	wg       sync.WaitGroup
}

func NewCollector(store *Store, source Source, now func() time.Time) *Collector {
	if now == nil {
		now = time.Now
	}
	return &Collector{store: store, source: source, now: now, next: map[string]time.Time{}, last: map[string]time.Time{}, blocked: map[string]time.Time{}, failures: map[string]int{}, running: map[string]bool{}, statuses: map[string]CollectionStatus{}, slots: make(chan struct{}, 3)}
}

func (c *Collector) Run(ctx context.Context) {
	c.Tick(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Tick(ctx)
		}
	}
}
func (c *Collector) Wait()                    { c.wg.Wait() }
func (c *Collector) Tick(ctx context.Context) { _ = c.refresh(ctx, nil, false) }
func (c *Collector) Refresh(ctx context.Context, ids []string) error {
	return c.refresh(ctx, ids, true)
}
func (c *Collector) refresh(ctx context.Context, ids []string, manual bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	accounts, err := c.source.Accounts(ctx)
	if err != nil {
		return errors.New("history credentials unavailable")
	}
	_ = c.store.SetAccounts(accounts)
	now := c.now()
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	for _, a := range accounts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.Disabled || a.Archived {
			continue
		}
		if len(ids) > 0 {
			matches := false
			for _, id := range a.CredentialIDs {
				matches = matches || selected[id]
			}
			if !matches {
				continue
			}
		}
		c.mu.Lock()
		due := (!now.Before(c.next[a.ID]) || manual) && !now.Before(c.last[a.ID].Add(time.Minute)) && !now.Before(c.blocked[a.ID]) && !c.running[a.ID]
		if due {
			select {
			case c.slots <- struct{}{}:
				c.running[a.ID] = true
				c.last[a.ID] = now
				c.wg.Add(1)
				go c.collect(ctx, a)
			default:
			}
		}
		c.mu.Unlock()
	}
	return nil
}

func (c *Collector) collect(ctx context.Context, a Account) {
	defer func() { <-c.slots; c.wg.Done() }()
	observations, err := c.source.Fetch(ctx, a)
	now := c.now()
	for _, o := range observations {
		if e := c.store.Observe(o); e != nil && err == nil {
			err = &FetchError{Code: "storage_degraded"}
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, a.ID)
	status := CollectionStatus{AccountID: a.ID, State: "ready"}
	h := fnv.New32a()
	_, _ = h.Write([]byte(a.ID))
	next := now.Add(15*time.Minute + time.Duration(h.Sum32()%31)*time.Second)
	for _, o := range observations {
		if remaining := o.ResetAt.Sub(now); remaining > 0 {
			candidate := o.ResetAt.Add(-10 * time.Minute)
			if remaining <= 10*time.Minute {
				candidate = now.Add(time.Minute)
			}
			if candidate.Before(next) {
				next = candidate
			}
		}
	}
	if err != nil {
		status.State = "unavailable"
		status.ErrorCode = "request_failed"
		c.failures[a.ID]++
		delay := 2 * time.Minute * time.Duration(1<<min(c.failures[a.ID]-1, 4))
		if delay > 30*time.Minute {
			delay = 30 * time.Minute
		}
		next = now.Add(delay)
		var failure *FetchError
		if errors.As(err, &failure) {
			status.ErrorCode = failure.Code
			if failure.RetryAt.After(next) {
				next = failure.RetryAt
			}
		}
		c.blocked[a.ID] = next
		status.RetryAt = &next
	} else {
		delete(c.failures, a.ID)
		delete(c.blocked, a.ID)
		if len(observations) == 0 {
			status.State = "unknown"
			status.ErrorCode = "no_weekly_window"
		}
	}
	c.statuses[a.ID] = status
	c.next[a.ID] = next
}

func (c *Collector) Snapshot(from, to time.Time) Snapshot {
	out := c.store.Snapshot(from, to)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range out.Accounts {
		if status, ok := c.statuses[a.ID]; ok {
			out.Collection = append(out.Collection, status)
		}
	}
	return out
}
