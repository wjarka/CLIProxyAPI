package quotahistory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type archive struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
	Cycles   []Cycle   `json:"cycles"`
}

type Store struct {
	mu         sync.Mutex
	path       string
	retention  time.Duration
	data       archive
	status     string
	loadFailed bool
	now        func() time.Time
}

func NewStore(path string, retention time.Duration) (*Store, error) {
	if retention <= 0 || retention > Retention {
		retention = Retention
	}
	s := &Store{path: path, retention: retention, status: "ready", now: time.Now, data: archive{Version: 1, Accounts: []Account{}, Cycles: []Cycle{}}}
	if path == "" {
		s.status = "unavailable"
		s.loadFailed = true
		return s, errors.New("history path unavailable")
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		defer func() { _ = f.Close() }()
		var data []byte
		data, err = io.ReadAll(io.LimitReader(f, MaxArchiveBytes+1))
		if err == nil && len(data) > MaxArchiveBytes {
			err = errors.New("history archive too large")
		}
		if err == nil {
			err = json.Unmarshal(data, &s.data)
		}
		if err == nil && (s.data.Version != 1 || len(s.data.Cycles) > MaxCycles || len(s.data.Accounts) > MaxCycles) {
			err = errors.New("unsupported history archive")
		}
		if err == nil {
			for _, c := range s.data.Cycles {
				if validObservation(c.PoolObservation) != nil {
					err = errors.New("invalid history archive")
					break
				}
			}
		}
	}
	if err != nil {
		s.data = archive{Version: 1, Accounts: []Account{}, Cycles: []Cycle{}}
		s.status = "unavailable"
		s.loadFailed = true
		return s, errors.New("cannot load history archive")
	}
	s.pruneLocked()
	return s, nil
}

func validObservation(o PoolObservation) error {
	if o.AccountID == "" || len(o.AccountID) > 256 || o.Provider == "" || len(o.Provider) > 64 || o.PoolID == "" || len(o.PoolID) > 256 || len(o.Label) > 256 || o.ObservedAt.IsZero() || o.ResetAt.IsZero() || !o.ObservedAt.Before(o.ResetAt) || o.ResetAt.Sub(o.ObservedAt) > 8*24*time.Hour {
		return errors.New("invalid weekly observation")
	}
	if p := o.RemainingPercent; p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > 100) {
		return errors.New("invalid remaining percentage")
	}
	if o.StartAt != nil && (!o.StartAt.Before(o.ResetAt) || o.StartAt.After(o.ObservedAt)) {
		return errors.New("invalid cycle start")
	}
	return nil
}

func cloneObservation(o PoolObservation) PoolObservation {
	if o.RemainingPercent != nil {
		p := *o.RemainingPercent
		o.RemainingPercent = &p
	}
	if o.StartAt != nil {
		at := *o.StartAt
		o.StartAt = &at
	}
	return o
}

func (s *Store) Observe(o PoolObservation) error {
	if err := validObservation(o); err != nil {
		return err
	}
	o = cloneObservation(o)
	s.mu.Lock()
	defer s.mu.Unlock()
	var previous *Cycle
	for i := range s.data.Cycles {
		c := &s.data.Cycles[i]
		if c.AccountID != o.AccountID || c.PoolID != o.PoolID {
			continue
		}
		if c.ResetAt.Equal(o.ResetAt) {
			if !o.ObservedAt.After(c.ObservedAt) || (o.RemainingPercent == nil && c.RemainingPercent != nil) {
				return nil
			}
			if o.StartAt == nil {
				o.StartAt = c.StartAt
			}
			c.PoolObservation = o
			return s.saveLocked()
		}
		if previous == nil || c.ObservedAt.After(previous.ObservedAt) {
			previous = c
		}
	}
	state := "open"
	if previous != nil {
		if !o.ObservedAt.After(previous.ObservedAt) {
			return nil
		}
		if previous.ResetAt.After(o.ObservedAt) {
			previous.State = "uncertain"
			state = "uncertain"
		} else {
			if previous.State != "uncertain" {
				previous.State = "completed"
				if o.StartAt == nil && o.ResetAt.Sub(previous.ResetAt) == 7*24*time.Hour {
					start := previous.ResetAt
					o.StartAt = &start
				}
			}
		}
	}
	s.data.Cycles = append(s.data.Cycles, Cycle{PoolObservation: o, FirstObservedAt: o.ObservedAt, State: state})
	return s.saveLocked()
}

// SetAccounts archives removed credentials while keeping their historical rows.
// Runtime credential IDs are only joins, never account identity proofs.
func (s *Store) SetAccounts(accounts []Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]Account, len(accounts))
	for _, a := range accounts {
		if a.ID != "" {
			a.CredentialIDs = append([]string{}, a.CredentialIDs...)
			next[a.ID] = a
		}
	}
	changed := false
	for i, a := range s.data.Accounts {
		if n, ok := next[a.ID]; ok {
			before, _ := json.Marshal(a)
			after, _ := json.Marshal(n)
			if string(before) != string(after) {
				changed = true
			}
			s.data.Accounts[i] = n
			delete(next, a.ID)
		} else if !a.Archived || len(a.CredentialIDs) > 0 {
			a.Archived = true
			a.CredentialIDs = []string{}
			s.data.Accounts[i] = a
			changed = true
		}
	}
	for _, a := range next {
		s.data.Accounts = append(s.data.Accounts, a)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) Snapshot(from, to time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	out := Snapshot{Accounts: []Account{}, Cycles: []Cycle{}, Collection: []CollectionStatus{}, StorageStatus: s.status, ObservedAt: now}
	needed := map[string]bool{}
	for _, c := range s.data.Cycles {
		if c.ResetAt.Before(now.Add(-s.retention)) || c.ResetAt.Before(from) || !c.FirstObservedAt.Before(to) {
			continue
		}
		c.PoolObservation = cloneObservation(c.PoolObservation)
		if c.State == "open" && !c.ResetAt.After(now) {
			c.State = "completed"
		}
		out.Cycles = append(out.Cycles, c)
		needed[c.AccountID] = true
	}
	for _, a := range s.data.Accounts {
		if !a.Archived || needed[a.ID] {
			a.CredentialIDs = append([]string{}, a.CredentialIDs...)
			out.Accounts = append(out.Accounts, a)
		}
	}
	sort.Slice(out.Cycles, func(i, j int) bool {
		a, b := out.Cycles[i], out.Cycles[j]
		if !a.ResetAt.Equal(b.ResetAt) {
			return a.ResetAt.Before(b.ResetAt)
		}
		return a.AccountID+a.PoolID < b.AccountID+b.PoolID
	})
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].ID < out.Accounts[j].ID })
	return out
}

func (s *Store) pruneLocked() {
	cutoff := s.now().Add(-s.retention)
	cycles := s.data.Cycles[:0]
	active := map[string]bool{}
	for _, c := range s.data.Cycles {
		if !c.ResetAt.Before(cutoff) {
			cycles = append(cycles, c)
			active[c.AccountID] = true
		}
	}
	s.data.Cycles = cycles
	if len(cycles) > MaxCycles {
		sort.Slice(cycles, func(i, j int) bool { return cycles[i].ObservedAt.Before(cycles[j].ObservedAt) })
		s.data.Cycles = cycles[len(cycles)-MaxCycles:]
	}
	accounts := s.data.Accounts[:0]
	for _, a := range s.data.Accounts {
		if !a.Archived || active[a.ID] {
			accounts = append(accounts, a)
		}
	}
	s.data.Accounts = accounts
}

func (s *Store) saveLocked() error {
	s.pruneLocked()
	if s.loadFailed {
		return errors.New("history storage unavailable")
	}
	if err := s.writeLocked(); err != nil {
		s.status = "degraded"
		return errors.New("cannot persist quota history")
	}
	s.status = "ready"
	return nil
}

func (s *Store) writeLocked() error {
	b, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	if len(b) > MaxArchiveBytes {
		return fmt.Errorf("history archive exceeds limit")
	}
	dir := filepath.Dir(s.path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".quota-history-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	return parent.Sync()
}
