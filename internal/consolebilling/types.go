// Package consolebilling keeps Console billing authentication separate from inference credentials.
// Console endpoints are undocumented and fixture-tested, not validated with live accounts.
package consolebilling

import "time"

type Provider string

const (
	Claude Provider = "claude"
	OpenAI Provider = "openai"
)

type Connection struct {
	ID         string    `json:"id"`
	Provider   Provider  `json:"provider"`
	Label      string    `json:"label"`
	Status     string    `json:"status"`
	AccountIDs []string  `json:"account_ids"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}
type Account struct {
	ID             string     `json:"id"`
	Provider       Provider   `json:"provider"`
	OrganizationID string     `json:"organization_id"`
	Name           string     `json:"name"`
	ConnectionID   string     `json:"connection_id"`
	Balance        *float64   `json:"balance"`
	Currency       string     `json:"currency"`
	Grants         []Grant    `json:"grants"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
}
type Grant struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	Amount    *float64   `json:"amount"`
	Remaining *float64   `json:"remaining"`
	ExpiresAt *time.Time `json:"expires_at"`
}
type Binding struct {
	CredentialID            string `json:"credential_id"`
	AccountID               string `json:"account_id"`
	Verified                bool   `json:"verified"`
	VerificationFingerprint string `json:"verification_fingerprint,omitempty"`
}
type Snapshot struct {
	Connections []Connection `json:"connections"`
	Accounts    []Account    `json:"accounts"`
	Bindings    []Binding    `json:"bindings"`
}

// Error contains a fixed safe code, never provider bodies or transport errors.
type Error struct{ Code string }

func (e *Error) Error() string  { return e.Code }
func failure(code string) error { return &Error{Code: code} }
