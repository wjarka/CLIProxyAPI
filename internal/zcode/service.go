package zcode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotConnected = errors.New("no ZCode account")
	ErrCardsChanged = errors.New("ZCode reset cards changed")
	ErrReconnect    = errors.New("ZCode account needs to reconnect")
	ErrUpstream     = errors.New("ZCode request failed")
	ErrStorage      = errors.New("ZCode account storage unavailable")
	ErrInvalid      = errors.New("invalid ZCode request")
)

type Account struct {
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

type AccountView struct {
	Connected bool     `json:"connected"`
	SessionID string   `json:"session_id,omitempty"`
	Account   *Account `json:"account,omitempty"`
}

type Login struct {
	ID              string `json:"id"`
	AuthorizeURL    string `json:"authorize_url"`
	ExpiresAt       int64  `json:"expires_at"`
	PollIntervalSec int    `json:"poll_interval_sec"`
}

type Cards struct {
	SessionID string  `json:"session_id"`
	FiveHour  []int64 `json:"five_hour"`
	Weekly    []int64 `json:"weekly"`
}

type UseRequest struct {
	SessionID      string `json:"session_id"`
	ResetType      string `json:"reset_type"`
	ExpiresAt      int64  `json:"expires_at"`
	AvailableCount int    `json:"available_count"`
	IdempotencyKey string `json:"idempotency_key"`
}

type savedAccount struct {
	SessionID string  `json:"session_id"`
	Identity  Account `json:"account"`
	ZCode     string  `json:"zcode_token"`
	ZAI       string  `json:"zai_token"`
}

type loginFlow struct {
	Login
	upstreamID string
	pollToken  string
	nextPoll   time.Time
	status     string
}

// Service manages the proxy owner's ZCode account and reset card inventory.
type Service struct {
	mu      sync.Mutex
	path    string
	baseURL string
	client  *http.Client
	now     func() time.Time
	flow    *loginFlow
}

func New(path string) *Service {
	return &Service{
		path: path, baseURL: "https://zcode.z.ai", now: time.Now,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

func (s *Service) Account() (AccountView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.loadAccount()
	if errors.Is(err, ErrNotConnected) {
		return AccountView{}, nil
	}
	if err != nil {
		return AccountView{}, err
	}
	return AccountView{Connected: true, SessionID: account.SessionID, Account: &account.Identity}, nil
}

func (s *Service) Start(ctx context.Context) (Login, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var secret [32]byte
	rand.Read(secret[:])
	pollToken := hex.EncodeToString(secret[:])
	var result struct {
		FlowID          string `json:"flow_id"`
		AuthorizeURL    string `json:"authorize_url"`
		ExpiresAt       int64  `json:"expires_at"`
		PollIntervalSec int    `json:"poll_interval_sec"`
	}
	status, code, err := s.request(ctx, http.MethodPost, "/api/v1/oauth/cli/init", pollToken, "",
		map[string]string{"provider": "zai"}, &result)
	now := s.now()
	expiresAt := time.Unix(result.ExpiresAt, 0)
	if err != nil || status != http.StatusOK || code != 0 || result.FlowID == "" ||
		!validAuthorizationURL(result.AuthorizeURL) || !expiresAt.After(now) {
		return Login{}, ErrUpstream
	}
	if limit := now.Add(20 * time.Minute); expiresAt.After(limit) {
		expiresAt = limit
	}
	login := Login{
		ID: rand.Text(), AuthorizeURL: result.AuthorizeURL, ExpiresAt: expiresAt.UnixMilli(),
		PollIntervalSec: min(30, max(1, result.PollIntervalSec)),
	}
	s.flow = &loginFlow{Login: login, upstreamID: result.FlowID, pollToken: pollToken, status: "pending"}
	return login, nil
}

func (s *Service) Poll(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flow := s.flow
	now := s.now()
	if flow == nil || flow.ID != id || flow.ExpiresAt <= now.UnixMilli() {
		if flow != nil && flow.ExpiresAt <= now.UnixMilli() {
			s.flow = nil
		}
		return "expired", nil
	}
	if flow.status != "pending" || now.Before(flow.nextPoll) {
		return flow.status, nil
	}
	flow.nextPoll = now.Add(time.Duration(flow.PollIntervalSec) * time.Second)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var result struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		User   struct {
			ID    string `json:"user_id"`
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
		ZAI struct {
			AccessToken      string `json:"access_token"`
			AccessTokenCamel string `json:"accessToken"`
		} `json:"zai"`
	}
	status, code, err := s.request(ctx, http.MethodGet, "/api/v1/oauth/cli/poll/"+url.PathEscape(flow.upstreamID),
		flow.pollToken, "", nil, &result)
	switch {
	case code == 3004:
		flow.finish("expired")
	case status == http.StatusTooManyRequests, status == http.StatusRequestTimeout, status >= 500, status == 0:
		return "pending", nil
	case err != nil, status != http.StatusOK, code != 0:
		flow.finish("failed")
	case result.Status == "pending":
		return "pending", nil
	case result.Status == "ready":
		token := strings.TrimSpace(result.ZAI.AccessToken)
		if token == "" {
			token = strings.TrimSpace(result.ZAI.AccessTokenCamel)
		}
		result.Token = strings.TrimSpace(result.Token)
		if result.Token == "" || token == "" || strings.TrimSpace(result.User.ID) == "" {
			flow.finish("failed")
			break
		}
		account := savedAccount{
			SessionID: rand.Text(), Identity: Account{DisplayName: result.User.Name, Email: result.User.Email},
			ZCode: result.Token, ZAI: token,
		}
		if err := s.saveAccount(account); err != nil {
			flow.finish("failed")
			return "", err
		}
		flow.finish("ready")
	default:
		flow.finish("failed")
	}
	return flow.status, nil
}

func (f *loginFlow) finish(status string) {
	f.status = status
	f.pollToken = ""
	f.upstreamID = ""
	f.AuthorizeURL = ""
}

func (s *Service) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flow != nil && s.flow.ID == id {
		s.flow = nil
	}
}

func (s *Service) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	s.flow = nil
	return nil
}

func (s *Service) Cards(ctx context.Context) (Cards, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.loadAccount()
	if err != nil {
		return Cards{}, err
	}
	return s.cards(ctx, account)
}

func (s *Service) Use(ctx context.Context, input UseRequest) error {
	if input.SessionID == "" || input.ExpiresAt <= 0 || input.AvailableCount <= 0 ||
		(input.ResetType != "FIVE_HOUR" && input.ResetType != "WEEK") || !validIdempotencyKey(input.IdempotencyKey) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.loadAccount()
	if err != nil {
		return err
	}
	if account.SessionID != input.SessionID {
		return ErrCardsChanged
	}
	cards, err := s.cards(ctx, account)
	if err != nil {
		return err
	}
	available := cards.FiveHour
	if input.ResetType == "WEEK" {
		available = cards.Weekly
	}
	if len(available) != input.AvailableCount || available[0] != input.ExpiresAt {
		return ErrCardsChanged
	}
	var result struct {
		Used bool `json:"used"`
	}
	status, code, err := s.request(ctx, http.MethodPost, "/api/v1/coding-plan/reset/use", account.ZCode, account.ZAI,
		map[string]string{"idempotency_key": input.IdempotencyKey, "reset_type": input.ResetType}, &result)
	if err := resetError(status, code, err); err != nil {
		return err
	}
	if !result.Used {
		return ErrCardsChanged
	}
	return nil
}

type resetCard struct {
	ExpireAt int64 `json:"expire_at"`
}

func (s *Service) cards(ctx context.Context, account savedAccount) (Cards, error) {
	var result struct {
		FiveHour []resetCard `json:"available_five_hour_resets"`
		Weekly   []resetCard `json:"available_week_resets"`
	}
	status, code, err := s.request(ctx, http.MethodGet, "/api/v1/coding-plan/reset/status", account.ZCode, account.ZAI, nil, &result)
	if err := resetError(status, code, err); err != nil {
		return Cards{}, err
	}
	if result.FiveHour == nil || result.Weekly == nil {
		return Cards{}, ErrUpstream
	}
	now := s.now().UnixMilli()
	return Cards{SessionID: account.SessionID, FiveHour: activeExpiries(result.FiveHour, now), Weekly: activeExpiries(result.Weekly, now)}, nil
}

func activeExpiries(cards []resetCard, now int64) []int64 {
	expiries := make([]int64, 0, len(cards))
	for _, card := range cards {
		expiry := card.ExpireAt
		if expiry > 0 && expiry < 1e11 {
			expiry *= 1000
		}
		if expiry > now {
			expiries = append(expiries, expiry)
		}
	}
	slices.Sort(expiries)
	return expiries
}

func validAuthorizationURL(raw string) bool {
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" || target.User != nil || (target.Port() != "" && target.Port() != "443") {
		return false
	}
	host := strings.ToLower(target.Hostname())
	return host == "z.ai" || strings.HasSuffix(host, ".z.ai")
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 64 {
		return false
	}
	for _, char := range key {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func (s *Service) loadAccount() (savedAccount, error) {
	var account savedAccount
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return account, ErrNotConnected
	}
	if err != nil || json.Unmarshal(data, &account) != nil || account.SessionID == "" || account.ZCode == "" || account.ZAI == "" {
		return savedAccount{}, ErrStorage
	}
	return account, nil
}

func (s *Service) saveAccount(account savedAccount) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ErrStorage
	}
	temp, err := os.CreateTemp(dir, ".account-*")
	if err != nil {
		return ErrStorage
	}
	defer os.Remove(temp.Name())
	writeErr := json.NewEncoder(temp).Encode(account)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(temp.Name(), s.path) != nil {
		return ErrStorage
	}
	return nil
}
