package consolebilling

import (
	"context"
	"errors"
	"time"
)

var errRefreshSkipped = errors.New("background refresh skipped")

// RunRefreshLoop refreshes active connections immediately and periodically. It
// returns only after all started refreshes have exited.
func (m *Manager) RunRefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	started := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	m.runRefreshLoopAt(ctx, interval, ticker.C, started)
}

type refreshSchedule struct {
	next          time.Time
	failures      uint
	manualSuccess uint64
}
type refreshResult struct {
	id            string
	err           error
	started       time.Time
	manualSuccess uint64
}

func (m *Manager) runRefreshLoop(ctx context.Context, interval time.Duration, ticks <-chan time.Time, now func() time.Time) {
	m.runRefreshLoopAt(ctx, interval, ticks, now())
}

func (m *Manager) runRefreshLoopAt(ctx context.Context, interval time.Duration, ticks <-chan time.Time, initial time.Time) {
	schedules := make(map[string]refreshSchedule)
	inFlight := make(map[string]bool)
	pending := make(map[string]time.Time)
	results := make(chan refreshResult, 4)
	scan := func(at time.Time) {
		m.mu.Lock()
		present := make(map[string]bool)
		for _, c := range m.state.Snapshot.Connections {
			if c.Status == "reconnect_required" || m.state.Sessions[c.ID] == "" {
				continue
			}
			present[c.ID] = true
			schedule := schedules[c.ID]
			if schedule.manualSuccess != m.manualSuccess[c.ID] {
				schedule = refreshSchedule{manualSuccess: m.manualSuccess[c.ID]}
				schedules[c.ID] = schedule
			}
			if !inFlight[c.ID] && !at.Before(schedules[c.ID].next) {
				pending[c.ID] = at
			}
		}
		m.mu.Unlock()
		for id := range schedules {
			if !present[id] && !inFlight[id] {
				delete(schedules, id)
			}
		}
		for id := range pending {
			if !present[id] {
				delete(pending, id)
			}
		}
	}
	dispatch := func() {
		for id, started := range pending {
			if len(inFlight) == 4 || ctx.Err() != nil {
				return
			}
			delete(pending, id)
			inFlight[id] = true
			manualSuccess := schedules[id].manualSuccess
			go func(id string) {
				err := m.refresh(ctx, id, true)
				results <- refreshResult{id, err, started, manualSuccess}
			}(id)
		}
	}
	scan(initial)
	for {
		dispatch()
		if ctx.Err() != nil {
			for len(inFlight) > 0 {
				result := <-results
				delete(inFlight, result.id)
			}
			return
		}
		select {
		case <-ctx.Done():
		case tick, ok := <-ticks:
			if !ok {
				for len(inFlight) > 0 {
					result := <-results
					delete(inFlight, result.id)
				}
				return
			}
			scan(tick)
		case result := <-results:
			delete(inFlight, result.id)
			schedule := schedules[result.id]
			m.mu.Lock()
			manualSuccess := m.manualSuccess[result.id]
			m.mu.Unlock()
			if manualSuccess != result.manualSuccess {
				schedules[result.id] = refreshSchedule{manualSuccess: manualSuccess}
				continue
			}
			if result.err == nil {
				schedule.failures = 0
			} else if result.err.Error() == "provider_unavailable" || result.err.Error() == "rate_limited" {
				if schedule.failures < 5 {
					schedule.failures++
				}
			} else if !errors.Is(result.err, errRefreshSkipped) && result.err.Error() != "superseded" {
				schedule.failures = 0
			}
			delay := interval
			for n := uint(1); n < schedule.failures && n < 5; n++ {
				delay *= 2
			}
			schedule.next = result.started.Add(delay)
			schedules[result.id] = schedule
		}
	}
}
