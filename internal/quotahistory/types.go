// Package quotahistory retains quota observations independently of routing state.
package quotahistory

import "time"

const Retention = 52 * 7 * 24 * time.Hour
const MaxArchiveBytes = 16 << 20
const MaxCycles = 32768

type PoolObservation struct {
	AccountID        string     `json:"account_id"`
	Provider         string     `json:"provider"`
	PoolID           string     `json:"pool_id"`
	Label            string     `json:"label"`
	ObservedAt       time.Time  `json:"observed_at"`
	ResetAt          time.Time  `json:"reset_at"`
	StartAt          *time.Time `json:"start_at,omitempty"`
	RemainingPercent *float64   `json:"remaining_percent"`
}

type Account struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Label         string   `json:"label"`
	CredentialIDs []string `json:"credential_ids"`
	Archived      bool     `json:"archived"`
	Disabled      bool     `json:"disabled"`
}

type Cycle struct {
	PoolObservation
	FirstObservedAt time.Time `json:"first_observed_at"`
	State           string    `json:"state"`
}

type CollectionStatus struct {
	AccountID string     `json:"account_id"`
	State     string     `json:"state"`
	ErrorCode string     `json:"error_code,omitempty"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
}

type Snapshot struct {
	Accounts      []Account          `json:"accounts"`
	Cycles        []Cycle            `json:"cycles"`
	Collection    []CollectionStatus `json:"collection"`
	StorageStatus string             `json:"storage_status"`
	ObservedAt    time.Time          `json:"observed_at"`
}
