package cliproxy

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// consoleRoutingProjection carries verified organization membership separately
// from observation freshness. It never publishes Console sessions or key material.
func consoleRoutingProjection(snapshot consolebilling.Snapshot) coreauth.ConsoleRoutingSnapshot {
	out := make(coreauth.ConsoleRoutingSnapshot)
	accounts := make(map[string]consolebilling.Account, len(snapshot.Accounts))
	connections := make(map[string]consolebilling.Connection, len(snapshot.Connections))
	for _, a := range snapshot.Accounts {
		accounts[a.ID] = a
	}
	for _, c := range snapshot.Connections {
		connections[c.ID] = c
	}
	for _, b := range snapshot.Bindings {
		if !b.Verified || b.CredentialID == "" || b.VerificationFingerprint == "" {
			continue
		}
		a, ok := accounts[b.AccountID]
		if !ok || a.OrganizationID == "" || a.ID != string(a.Provider)+":"+a.OrganizationID || (a.Provider != consolebilling.Claude && a.Provider != consolebilling.OpenAI) {
			continue
		}
		observation := coreauth.ConsoleRoutingObservation{Provider: string(a.Provider), AccountID: a.ID, CredentialFingerprint: b.VerificationFingerprint}
		if a.ObservedAt != nil {
			observation.ObservedAt = *a.ObservedAt
		}
		c, connected := connections[a.ConnectionID]
		owns := false
		for _, id := range c.AccountIDs {
			if id == a.ID {
				owns = true
				break
			}
		}
		balanceUsable := a.Balance == nil || (!math.IsNaN(*a.Balance) && !math.IsInf(*a.Balance, 0) && *a.Balance > 0)
		if connected && owns && c.Provider == a.Provider && c.Status == "ready" && c.Error == "" && a.Error == "" && (a.Status == "ready" || a.Status == "unknown") && a.ObservedAt != nil && !a.ObservedAt.IsZero() && balanceUsable {
			observation.ValidUntil = a.ObservedAt.Add(consolebilling.FreshnessWindow)
		}
		for _, g := range a.Grants {
			grant := coreauth.ConsoleRoutingGrant{}
			if g.Remaining != nil && !math.IsNaN(*g.Remaining) && !math.IsInf(*g.Remaining, 0) {
				grant.RemainingKnown = true
				grant.Remaining = *g.Remaining
			}
			if g.ExpiresAt != nil {
				grant.ExpiresAt = *g.ExpiresAt
			}
			observation.Grants = append(observation.Grants, grant)
		}
		out[b.CredentialID] = observation
	}
	return out
}

// startConsoleBilling publishes cached observations before accepting requests and
// shares one encrypted store between the scheduler and all management operations.
func (s *Service) startConsoleBilling(ctx context.Context) []api.ServerOption {
	s.consoleBillingMu.Lock()
	defer s.consoleBillingMu.Unlock()
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if (cfg != nil && cfg.Home.Enabled) || s.coreManager == nil || strings.TrimSpace(s.configPath) == "" {
		return nil
	}
	if s.consoleBilling == nil {
		manager, err := consolebilling.NewManager(filepath.Join(filepath.Dir(s.configPath), "console-billing"), helps.NewProxyAwareHTTPClient(ctx, cfg, nil, 0))
		if err != nil {
			log.Warn("Console billing storage unavailable; continuing without grant routing observations")
			return []api.ServerOption{api.WithConsoleBillingManager(nil)}
		}
		s.consoleBilling = manager
	}
	manager := s.consoleBilling
	if s.consoleBillingCancel == nil {
		workerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		refreshed := make(chan struct{})
		s.consoleBillingCancel = cancel
		s.consoleBillingDone = done
		s.coreManager.SetConsoleRoutingSnapshot(consoleRoutingProjection(manager.List()))
		go func() { defer close(refreshed); manager.RunRefreshLoop(workerCtx, time.Minute) }()
		go func() {
			defer close(done)
			defer s.coreManager.SetConsoleRoutingSnapshot(nil)
			for {
				select {
				case <-workerCtx.Done():
					<-refreshed
					return
				case <-manager.Changes():
					if workerCtx.Err() == nil {
						s.coreManager.SetConsoleRoutingSnapshot(consoleRoutingProjection(manager.List()))
					}
				}
			}
		}()
	}
	return []api.ServerOption{api.WithConsoleBillingManager(manager)}
}

func (s *Service) stopConsoleBilling(ctx context.Context) error {
	s.consoleBillingMu.Lock()
	cancel, done := s.consoleBillingCancel, s.consoleBillingDone
	s.consoleBillingMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
