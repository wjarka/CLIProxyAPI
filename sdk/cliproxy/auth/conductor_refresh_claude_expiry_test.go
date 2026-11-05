package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestClaudeExpiredRefreshTokenStopsRetries(t *testing.T) {
	for _, message := range []string{
		`{"error":"invalid_grant","error_description":"Refresh token expired"}`,
		`{"error":"invalid_grant","error_description":"refresh_token_expired"}`,
	} {
		t.Run(message, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			executor := &mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: message}}
			manager.RegisterExecutor(executor)
			auth := &Auth{ID: "work", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
				"access_token": "expired", "refresh_token": "expired-refresh", "expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
			}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			_, _ = manager.refreshAuthForRequest(context.Background(), auth.ID, "")
			current, _ := manager.GetByID(auth.ID)
			if !HasUnauthorizedAuthFailure(current) || manager.shouldRefresh(current, time.Now().Add(24*time.Hour)) {
				t.Fatalf("expired refresh token should require sign-in without scheduled retries: %+v", current)
			}
			_, _ = manager.refreshAuthForRequest(context.Background(), auth.ID, "")
			if got := executor.refreshCalls.Load(); got != 1 {
				t.Fatalf("refresh calls = %d, want 1", got)
			}
		})
	}
}

func TestClaudeExpiredRefreshPreservesValidAccessAndRecoversAfterLogin(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: `invalid_grant: Refresh token expired`}}
	manager.RegisterExecutor(executor)
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	auth := &Auth{ID: "work", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"access_token": "still-valid", "refresh_token": "expired-refresh", "expires_at": expires.Format(time.RFC3339),
	}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.refreshAuthForRequest(context.Background(), auth.ID, "")
	current, _ := manager.GetByID(auth.ID)
	if current.Unavailable || !current.NextRefreshAfter.Equal(expires) {
		t.Fatalf("retain usable access until expiry, got unavailable=%v next=%v", current.Unavailable, current.NextRefreshAfter)
	}
	// Successful inference clears LastError, but must not revive the bad refresh token.
	manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Model: "claude-test", Success: true})
	// A concurrent refresh must reuse the terminal answer, not call Anthropic again.
	_, _ = manager.refreshAuthForRequest(context.Background(), auth.ID, "")
	if got := executor.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	// Simulate expiry without waiting for the clock.
	manager.mu.Lock()
	manager.auths[auth.ID].Metadata["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	manager.mu.Unlock()
	_, _ = manager.refreshAuthForRequest(context.Background(), auth.ID, "")
	current, _ = manager.GetByID(auth.ID)
	if !HasUnauthorizedAuthFailure(current) || executor.refreshCalls.Load() != 1 {
		t.Fatal("expired access should require sign-in without retrying the expired refresh token")
	}
	current.Metadata["access_token"] = "new-login"
	current.Metadata["refresh_token"] = "new-refresh"
	current.Metadata["expires_at"] = expires.Format(time.RFC3339)
	current, err := manager.Update(context.Background(), current)
	if err != nil || current.Unavailable || current.LastError != nil {
		t.Fatalf("new login must recover the account: auth=%+v err=%v", current, err)
	}
	executor.errToReturn = nil
	if _, err := manager.refreshAuthForRequest(context.Background(), auth.ID, ""); err != nil {
		t.Fatalf("new credentials must be refreshable: %v", err)
	}
}

func TestClaudeOtherInvalidGrantRemainsRetryable(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: "invalid_grant: rotation race"}}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &Auth{ID: "work", Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.refreshAuthForRequest(context.Background(), "work", "")
	current, _ := manager.GetByID("work")
	if HasUnauthorizedAuthFailure(current) || current.NextRefreshAfter.IsZero() {
		t.Fatal("other invalid_grant responses must keep their existing retry behavior")
	}
}

func TestClaudeExpiredRefreshRejectsAnUpstreamRejectedAccessToken(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: "invalid_grant: Refresh token expired"}}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &Auth{ID: "work", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"access_token": "rejected", "refresh_token": "expired-refresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.refreshAuthForRequest(context.Background(), "work", "rejected")
	current, _ := manager.GetByID("work")
	if !HasUnauthorizedAuthFailure(current) {
		t.Fatal("a rejected access token must not be retained based on its future expiry")
	}
}

func TestClaudeExpiredRefreshCanBeExplicitlyRetried(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: "invalid_grant: Refresh token expired"}}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &Auth{ID: "work", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"access_token": "valid", "refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.refreshAuthForRequest(context.Background(), "work", "")
	executor.errToReturn = nil
	if _, err := manager.refreshAuthForRequestAtEpoch(withForceRefresh(context.Background()), "work", "", 0); err != nil {
		t.Fatal(err)
	}
	current, _ := manager.GetByID("work")
	if current.ExpiredClaudeRefreshToken != "" || current.LastError != nil || current.Unavailable {
		t.Fatal("a successful explicit refresh must clear the terminal renewal marker")
	}
	_, _ = manager.refreshAuthForRequest(context.Background(), "work", "")
	if got := executor.refreshCalls.Load(); got != 3 {
		t.Fatalf("normal renewal must resume after successful explicit refresh, got %d calls", got)
	}
}

func TestClaudeExpiredRefreshSurvivesConcurrentSave(t *testing.T) {
	ctx := context.Background()
	store := &reloadAuthStore{records: make(map[string]*Auth), saveEntered: make(chan struct{}), saveRelease: make(chan struct{})}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	executor := &mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: "invalid_grant: Refresh token expired"}}
	manager.RegisterExecutor(executor)
	current, err := manager.Register(WithSkipPersist(ctx), &Auth{ID: "work", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"access_token": "valid", "refresh_token": "expired-refresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	current.Label = "new label"
	done := make(chan error, 1)
	go func() {
		_, errUpdate := manager.Update(ctx, current)
		done <- errUpdate
	}()
	<-store.saveEntered
	_, _ = manager.refreshAuthForRequest(ctx, "work", "")
	close(store.saveRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, _ = manager.refreshAuthForRequest(ctx, "work", "")
	if got := executor.refreshCalls.Load(); got != 1 {
		t.Fatalf("concurrent save lost terminal renewal state, refresh calls = %d, want 1", got)
	}
}

type claudeRefreshCallbackExecutor struct {
	mockOAuthErrorExecutor
	beforeRefresh func()
}

func (e *claudeRefreshCallbackExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.beforeRefresh != nil {
		e.beforeRefresh()
	}
	return e.mockOAuthErrorExecutor.Refresh(ctx, auth)
}

func TestClaudeForcedRefreshClearsRejectionDespiteConcurrentInferenceError(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &claudeRefreshCallbackExecutor{mockOAuthErrorExecutor: mockOAuthErrorExecutor{id: "claude", errToReturn: oauthStatusError{code: http.StatusBadRequest, msg: "invalid_grant: Refresh token expired"}}}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(ctx, &Auth{ID: "work", Provider: "claude", Status: StatusActive, Metadata: map[string]any{
		"access_token": "valid", "refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.refreshAuthForRequest(ctx, "work", "")
	executor.errToReturn = nil
	executor.beforeRefresh = func() {
		manager.MarkResult(ctx, Result{AuthID: "work", Model: "claude-test", Error: &Error{HTTPStatus: 503, Message: "temporarily unavailable"}})
	}
	if _, err := manager.refreshAuthForRequestAtEpoch(withForceRefresh(ctx), "work", "", 0); err != nil {
		t.Fatal(err)
	}
	current, _ := manager.GetByID("work")
	if current.LastError == nil || current.LastError.HTTPStatus != 503 {
		t.Fatal("concurrent inference error should be preserved")
	}
	executor.beforeRefresh = nil
	_, _ = manager.refreshAuthForRequest(ctx, "work", "")
	if got := executor.refreshCalls.Load(); got != 3 {
		t.Fatalf("successful forced refresh must clear rejection despite the concurrent error, calls = %d, want 3", got)
	}
}
