package management

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/consolebilling"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

//go:embed assets/console-login.py
var consoleLoginHelper []byte

type consoleBillingCredential struct {
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Provider           string `json:"provider"`
	OrganizationID     string `json:"organization_id,omitempty"`
	VerificationStatus string `json:"verification_status"`
}

func (h *Handler) consoleBillingManager() (*consolebilling.Manager, error) {
	h.consoleBillingMu.Lock()
	defer h.consoleBillingMu.Unlock()
	if h.consoleBilling != nil {
		return h.consoleBilling, nil
	}
	h.mu.Lock()
	path, cfg := h.configFilePath, h.cfg
	h.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("billing storage is unavailable")
	}
	client := h.billingHTTPClient
	if client == nil {
		client = helps.NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	}
	manager, err := consolebilling.NewManager(filepath.Join(filepath.Dir(path), "console-billing"), client)
	if err != nil {
		return nil, err
	}
	h.consoleBilling = manager
	return manager, nil
}

func (h *Handler) billingManagerForRequest(c *gin.Context) *consolebilling.Manager {
	c.Header("Cache-Control", "no-store")
	manager, err := h.consoleBillingManager()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing_storage_unavailable"})
		return nil
	}
	return manager
}

func billingRequestJSON(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return false
	}
	return true
}

func consoleBillingOperationError(c *gin.Context, err error) {
	// Only fixed safe codes cross the management boundary. An expired Console
	// session must never produce management 401, which logs out the UI.
	code, status := "console_operation_failed", http.StatusBadGateway
	var safe *consolebilling.Error
	if errors.As(err, &safe) {
		switch safe.Code {
		case "invalid_provider", "invalid_session", "invalid_label", "invalid_binding", "no_organizations":
			code, status = safe.Code, http.StatusBadRequest
		case "not_found":
			code, status = safe.Code, http.StatusNotFound
		case "organization_mismatch", "superseded", "storage_conflict", "storage_busy":
			code, status = safe.Code, http.StatusConflict
		case "session_expired", "permission_denied", "rate_limited", "provider_unavailable", "unsupported_endpoint", "invalid_response", "unsupported_currency", "redirect_rejected", "storage_error", "storage_corrupt":
			code = safe.Code
		}
	}
	c.JSON(status, gin.H{"error": code})
}

func (h *Handler) GetConsoleBilling(c *gin.Context) {
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	snapshot := manager.List()
	credentials := make([]consoleBillingCredential, 0)
	h.mu.Lock()
	authManager := h.authManager
	h.mu.Unlock()
	if authManager != nil {
		for _, auth := range authManager.List() {
			provider, ok := billingCredentialProvider(auth)
			if !ok {
				continue
			}
			label := auth.Label
			if label == "" {
				label = auth.ID
			}
			credential := consoleBillingCredential{ID: auth.ID, Label: label, Provider: provider, VerificationStatus: "unlinked"}
			for _, binding := range snapshot.Bindings {
				if binding.CredentialID == auth.ID {
					credential.VerificationStatus = "unverified"
					if binding.Verified {
						credential.VerificationStatus = "verified"
					}
					for _, account := range snapshot.Accounts {
						if account.ID == binding.AccountID {
							credential.OrganizationID = account.OrganizationID
							break
						}
					}
					break
				}
			}
			credentials = append(credentials, credential)
		}
	}
	// A saved verification applies to the credential and organization selectors
	// observed then, including across configuration reloads and server restarts.
	for i := range snapshot.Bindings {
		binding := &snapshot.Bindings[i]
		auth, provider, ok := h.billingCredential(binding.CredentialID)
		binding.Verified = binding.Verified && ok && binding.VerificationFingerprint != "" && binding.VerificationFingerprint == billingCredentialFingerprint(auth, provider)
		binding.VerificationFingerprint = ""
		for j := range credentials {
			if credentials[j].ID == binding.CredentialID && !binding.Verified {
				credentials[j].VerificationStatus = "unverified"
			}
		}
	}
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].ID < credentials[j].ID })
	c.JSON(http.StatusOK, gin.H{"connections": snapshot.Connections, "accounts": snapshot.Accounts, "bindings": snapshot.Bindings, "credentials": credentials})
}

func (h *Handler) ConnectConsoleBilling(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		Provider string `json:"provider"`
		Label    string `json:"label"`
		Session  string `json:"session"`
	}
	if !billingRequestJSON(c, &input) {
		return
	}
	if (input.Provider != "claude" && input.Provider != "openai") || strings.TrimSpace(input.Session) == "" || strings.TrimSpace(input.Label) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_connection"})
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	connection, err := manager.Connect(c.Request.Context(), consolebilling.Provider(input.Provider), input.Label, input.Session)
	input.Session = ""
	if err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"connection": connection})
}

func (h *Handler) ReconnectConsoleBilling(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		Session string `json:"session"`
	}
	if !billingRequestJSON(c, &input) {
		return
	}
	if strings.TrimSpace(input.Session) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_connection"})
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	connection, err := manager.Reconnect(c.Request.Context(), c.Param("id"), input.Session)
	input.Session = ""
	if err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"connection": connection})
}

func (h *Handler) DeleteConsoleBilling(c *gin.Context) {
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	if err := manager.DeleteConnection(c.Param("id")); err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) RefreshConsoleBilling(c *gin.Context) {
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	if err := manager.Refresh(c.Request.Context(), c.Param("id")); err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) BindConsoleBilling(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		CredentialID string `json:"credential_id"`
		AccountID    string `json:"account_id"`
	}
	if !billingRequestJSON(c, &input) {
		return
	}
	auth, provider, ok := h.billingCredential(input.CredentialID)
	if !ok || auth == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_credential"})
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	found := false
	for _, account := range manager.List().Accounts {
		if account.ID == input.AccountID && string(account.Provider) == provider {
			found = true
			break
		}
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_billing_account"})
		return
	}
	if err := manager.Bind(input.CredentialID, input.AccountID, false); err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) UnbindConsoleBilling(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id := strings.TrimSpace(c.Query("credential_id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_credential"})
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	if err := manager.Unbind(id); err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) VerifyConsoleBillingCredential(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		CredentialID string `json:"credential_id"`
	}
	if !billingRequestJSON(c, &input) {
		return
	}
	auth, provider, ok := h.billingCredential(input.CredentialID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_credential"})
		return
	}
	manager := h.billingManagerForRequest(c)
	if manager == nil {
		return
	}
	organization, err := h.verifyBillingOrganization(c.Request.Context(), auth, provider)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "organization_verification_failed"})
		return
	}
	accountID := ""
	for _, account := range manager.List().Accounts {
		if string(account.Provider) == provider && account.OrganizationID == organization {
			accountID = account.ID
			break
		}
	}
	if accountID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "organization_not_connected"})
		return
	}
	// Check the current credential again after acquisition, to avoid binding a
	// replacement credential to an observation from a deleted/reloaded key.
	current, currentProvider, exists := h.billingCredential(input.CredentialID)
	if !exists || currentProvider != provider || current.RegistrationEpoch != auth.RegistrationEpoch || billingCredentialFingerprint(current, currentProvider) != billingCredentialFingerprint(auth, provider) {
		c.JSON(http.StatusConflict, gin.H{"error": "credential_changed"})
		return
	}
	if err := manager.BindVerified(input.CredentialID, accountID, billingCredentialFingerprint(auth, provider)); err != nil {
		consoleBillingOperationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"binding": consolebilling.Binding{CredentialID: input.CredentialID, AccountID: accountID, Verified: true}})
}

func (h *Handler) billingCredential(id string) (*coreauth.Auth, string, bool) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil, "", false
	}
	auth, ok := manager.GetByID(id)
	if !ok {
		return nil, "", false
	}
	provider, ok := billingCredentialProvider(auth)
	return auth, provider, ok
}

func billingCredentialProvider(auth *coreauth.Auth) (string, bool) {
	if auth == nil || strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return "", false
	}
	raw := strings.TrimSpace(auth.Attributes["base_url"])
	if strings.EqualFold(auth.Provider, "claude") && officialBillingBase(raw, "api.anthropic.com", true) {
		return "claude", true
	}
	if raw != "" && officialBillingBase(raw, "api.openai.com", false) {
		return "openai", true
	}
	return "", false
}

func officialBillingBase(raw, host string, allowEmpty bool) bool {
	if raw == "" {
		return allowEmpty
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, host) || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "" || path == "/v1"
}

func billingCredentialFingerprint(auth *coreauth.Auth, provider string) string {
	if auth == nil {
		return ""
	}
	values := []string{provider, auth.Provider, auth.Attributes["api_key"], auth.Attributes["base_url"]}
	var headers []string
	for key, value := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			headers = append(headers, key+"="+value)
		}
	}
	sort.Strings(headers)
	values = append(values, headers...)
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Organization/project selectors are the only custom headers allowed on this
// read-only probe. Dynamic selectors cannot be verified without an inference request.
func billingOrganizationSelectors(auth *coreauth.Auth) ([2]string, error) {
	var selectors [2]string
	if auth == nil {
		return selectors, errors.New("missing credential")
	}
	for key, value := range auth.Attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
		index := -1
		if strings.EqualFold(name, "OpenAI-Organization") {
			index = 0
		}
		if strings.EqualFold(name, "OpenAI-Project") {
			index = 1
		}
		if index < 0 || strings.TrimSpace(value) == "" {
			continue
		}
		if strings.ContainsAny(value, "$\r\n\t /\\") || len(value) > 200 {
			return selectors, errors.New("unverifiable organization selector")
		}
		if selectors[index] != "" && selectors[index] != value {
			return selectors, errors.New("ambiguous organization selector")
		}
		selectors[index] = value
	}
	return selectors, nil
}

func (h *Handler) verifyBillingOrganization(ctx context.Context, auth *coreauth.Auth, provider string) (string, error) {
	host, header := "api.anthropic.com", "anthropic-organization-id"
	if provider == "openai" {
		host, header = "api.openai.com", "openai-organization"
	} else if provider != "claude" {
		return "", errors.New("unsupported billing provider")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v1/models", nil)
	if err != nil {
		return "", errors.New("verification request failed")
	}
	if provider == "claude" {
		request.Header.Set("x-api-key", auth.Attributes["api_key"])
		request.Header.Set("anthropic-version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+auth.Attributes["api_key"])
		selectors, err := billingOrganizationSelectors(auth)
		if err != nil {
			return "", err
		}
		if selectors[0] != "" {
			request.Header.Set("OpenAI-Organization", selectors[0])
		}
		if selectors[1] != "" {
			request.Header.Set("OpenAI-Project", selectors[1])
		}
	}
	for key, value := range auth.Attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
		if (strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "x-api-key")) && value != "" && value != request.Header.Get(name) {
			return "", errors.New("unverifiable authentication override")
		}
	}
	request.Header.Set("Accept", "application/json")
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	client := h.billingHTTPClient
	if client == nil {
		client = helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	}
	isolated := *client
	isolated.Jar = nil
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := isolated.Do(request)
	if err != nil {
		return "", errors.New("verification request failed")
	}
	if response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("verification returned status %d", response.StatusCode)
	}
	organization := strings.TrimSpace(response.Header.Get(header))
	if organization == "" || len(organization) > 200 || strings.ContainsAny(organization, "\r\n\t /\\") {
		return "", errors.New("organization identifier unavailable")
	}
	if requested := request.Header.Get("OpenAI-Organization"); requested != "" && requested != organization {
		return "", errors.New("organization selector mismatch")
	}
	return organization, nil
}

func (h *Handler) DownloadConsoleLoginHelper(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", `attachment; filename="console-login.py"`)
	c.Data(http.StatusOK, "text/x-python; charset=utf-8", consoleLoginHelper)
}
