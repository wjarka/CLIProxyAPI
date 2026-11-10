package consolebilling

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type diskState struct {
	Version  int               `json:"version"`
	Snapshot Snapshot          `json:"snapshot"`
	Sessions map[string]string `json:"sessions"`
}

// FreshnessWindow bounds how long a successful observation is displayed as current.
const FreshnessWindow = 5 * time.Minute

type Manager struct {
	mu            sync.Mutex
	dir           string
	key           []byte
	client        *http.Client
	state         diskState
	revision      [32]byte
	exists        bool
	order         uint64
	published     map[string]uint64
	generation    map[string]uint64
	changes       chan struct{}
	active        map[string]int
	manualSuccess map[string]uint64
}

func NewManager(dir string, client *http.Client) (*Manager, error) {
	if dir == "" {
		return nil, failure("storage_error")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, failure("storage_error")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, failure("storage_error")
	}
	if os.Chmod(dir, 0700) != nil {
		return nil, failure("storage_error")
	}
	unlock, err := lockStore(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.Jar = nil
	c.Timeout = 0
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	m := &Manager{dir: dir, client: &c, changes: make(chan struct{}, 1), active: make(map[string]int), manualSuccess: make(map[string]uint64), generation: make(map[string]uint64), published: make(map[string]uint64), state: diskState{Version: 1, Snapshot: Snapshot{Connections: []Connection{}, Accounts: []Account{}, Bindings: []Binding{}}, Sessions: make(map[string]string)}}
	statePath := filepath.Join(dir, "billing.enc")
	keyPath := filepath.Join(dir, "session.key")
	encrypted, readErr := privateRead(statePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, failure("storage_error")
	}
	key, keyErr := privateRead(keyPath)
	if errors.Is(keyErr, os.ErrNotExist) && errors.Is(readErr, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, failure("storage_error")
		}
		f, createErr := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr != nil {
			return nil, failure("storage_error")
		}
		_, writeErr := f.Write(key)
		syncErr := f.Sync()
		closeErr := f.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return nil, failure("storage_error")
		}
	} else if keyErr != nil || len(key) != 32 {
		return nil, failure("storage_corrupt")
	}
	m.key = key
	if readErr == nil {
		m.exists = true
		m.revision = sha256.Sum256(encrypted)
		plain, err := m.decrypt(encrypted)
		if err != nil || json.Unmarshal(plain, &m.state) != nil || m.state.Version != 1 || m.state.Sessions == nil {
			return nil, failure("storage_corrupt")
		}
		if err = m.validateState(); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func privateRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16<<20 {
		return nil, failure("storage_error")
	}
	return os.ReadFile(path)
}
func (m *Manager) validateState() error {
	if m.state.Snapshot.Connections == nil || m.state.Snapshot.Accounts == nil || m.state.Snapshot.Bindings == nil {
		return failure("storage_corrupt")
	}
	seen := make(map[string]bool)
	for _, c := range m.state.Snapshot.Connections {
		if c.ID == "" || seen[c.ID] || (c.Provider != Claude && c.Provider != OpenAI) || c.AccountIDs == nil {
			return failure("storage_corrupt")
		}
		seen[c.ID] = true
	}
	for id := range m.state.Sessions {
		if !seen[id] {
			return failure("storage_corrupt")
		}
	}
	accountIDs := make(map[string]bool)
	for _, a := range m.state.Snapshot.Accounts {
		if a.ID != string(a.Provider)+":"+a.OrganizationID || !organizationPattern.MatchString(a.OrganizationID) || accountIDs[a.ID] || a.Grants == nil {
			return failure("storage_corrupt")
		}
		accountIDs[a.ID] = true
	}
	for _, b := range m.state.Snapshot.Bindings {
		if b.CredentialID == "" || !accountIDs[b.AccountID] {
			return failure("storage_corrupt")
		}
	}
	return nil
}
func (m *Manager) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (m *Manager) decrypt(b []byte) ([]byte, error) {
	a, err := m.aead()
	if err != nil || len(b) < 3+a.NonceSize() || string(b[:3]) != "CB1" {
		return nil, failure("storage_corrupt")
	}
	return a.Open(nil, b[3:3+a.NonceSize()], b[3+a.NonceSize():], []byte("consolebilling-v1"))
}
func (m *Manager) persist() error {
	unlock, err := lockStore(m.dir)
	if err != nil {
		return err
	}
	defer unlock()
	current, readErr := privateRead(filepath.Join(m.dir, "billing.enc"))
	if m.exists {
		if readErr != nil || sha256.Sum256(current) != m.revision {
			return failure("storage_conflict")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return failure("storage_conflict")
	}

	plain, err := json.Marshal(m.state)
	if err != nil {
		return failure("storage_error")
	}
	a, err := m.aead()
	if err != nil {
		return failure("storage_error")
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return failure("storage_error")
	}
	out := append([]byte("CB1"), nonce...)
	out = a.Seal(out, nonce, plain, []byte("consolebilling-v1"))
	f, err := os.CreateTemp(m.dir, ".billing-*")
	if err != nil {
		return failure("storage_error")
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(out)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return failure("storage_error")
	}
	if os.Rename(name, filepath.Join(m.dir, "billing.enc")) != nil {
		return failure("storage_error")
	}
	m.exists = true
	m.revision = sha256.Sum256(out)
	// Rename is the commit point. A failed directory sync must not roll memory back after disk committed.
	if dir, err := os.Open(m.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
func (m *Manager) backup() diskState {
	b, _ := json.Marshal(m.state)
	var out diskState
	_ = json.Unmarshal(b, &out)
	return out
}

// Changes signals committed store changes, coalescing pending notifications.
func (m *Manager) Changes() <-chan struct{} { return m.changes }

func (m *Manager) save(before diskState) error {
	if err := m.persist(); err != nil {
		m.state = before
		return err
	}
	select {
	case m.changes <- struct{}{}:
	default:
	}
	return nil
}
func (m *Manager) List() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.backup().Snapshot
	now := time.Now()
	for i := range snapshot.Accounts {
		a := &snapshot.Accounts[i]
		if (a.Status == "ready" || a.Status == "unknown") && (a.ObservedAt != nil && now.Sub(*a.ObservedAt) > FreshnessWindow) {
			a.Status = "stale"
		}
	}
	return snapshot
}
func (m *Manager) connection(id string) (int, error) {
	for i, c := range m.state.Snapshot.Connections {
		if c.ID == id {
			return i, nil
		}
	}
	return -1, failure("not_found")
}
func (m *Manager) publish(id string, d discovery, order uint64) {
	ids := make([]string, 0, len(d.accounts))
	for _, a := range d.accounts {
		a.ConnectionID = id
		ids = append(ids, a.ID)
		if m.published[a.ID] > order {
			continue
		}
		found := false
		for i, existing := range m.state.Snapshot.Accounts {
			if existing.ID == a.ID {
				if a.Error != "" {
					// Another login's successful observation stays authoritative when this login lacks access.
					if existing.ConnectionID != id {
						found = true
						break
					}
					existing.Status = "stale"
					existing.Error = a.Error
					a = existing
				}
				m.state.Snapshot.Accounts[i] = a
				m.published[a.ID] = order
				found = true
				break
			}
		}
		if !found {
			m.state.Snapshot.Accounts = append(m.state.Snapshot.Accounts, a)
			m.published[a.ID] = order
		}
	}
	i, _ := m.connection(id)
	c := &m.state.Snapshot.Connections[i]
	c.AccountIDs = ids
	c.Status = "ready"
	c.Error = ""
	for _, a := range d.accounts {
		if a.Error != "" {
			c.Error = "partial_billing_access"
			break
		}
	}
	c.UpdatedAt = time.Now().UTC()
	m.state.Sessions[id] = d.session
}
func (m *Manager) Connect(ctx context.Context, provider Provider, label, session string) (Connection, error) {
	if len(label) > 512 {
		return Connection{}, failure("invalid_label")
	}
	m.mu.Lock()
	m.order++
	order := m.order
	m.mu.Unlock()
	d, err := m.discover(ctx, provider, session)
	if err != nil {
		return Connection{}, err
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		return Connection{}, failure("storage_error")
	}
	id := hex.EncodeToString(idBytes)
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return Connection{}, failure("provider_unavailable")
	}
	before := m.backup()
	m.state.Snapshot.Connections = append(m.state.Snapshot.Connections, Connection{ID: id, Provider: provider, Label: label, AccountIDs: []string{}})
	m.publish(id, d, order)
	if err = m.save(before); err != nil {
		return Connection{}, err
	}
	i, _ := m.connection(id)
	out := m.state.Snapshot.Connections[i]
	out.AccountIDs = append([]string{}, out.AccountIDs...)
	return out, nil
}
func continuity(c Connection, d discovery) bool {
	ids := make(map[string]bool)
	for _, a := range d.accounts {
		ids[a.ID] = true
	}
	for _, id := range c.AccountIDs {
		if !ids[id] {
			return false
		}
	}
	return true
}
func (m *Manager) Reconnect(ctx context.Context, id, session string) (Connection, error) {
	m.mu.Lock()
	i, err := m.connection(id)
	if err != nil {
		m.mu.Unlock()
		return Connection{}, err
	}
	c := m.state.Snapshot.Connections[i]
	m.active[id]++
	defer m.finishOperation(id)
	m.generation[id]++
	generation := m.generation[id]
	m.order++
	order := m.order
	m.mu.Unlock()
	d, err := m.discover(ctx, c.Provider, session)
	if err != nil {
		return Connection{}, err
	}
	if !continuity(c, d) {
		return Connection{}, failure("organization_mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i, err = m.connection(id)
	if err != nil {
		return Connection{}, err
	}
	if m.generation[id] != generation {
		return Connection{}, failure("superseded")
	}
	if ctx.Err() != nil {
		return Connection{}, failure("provider_unavailable")
	}
	before := m.backup()
	m.publish(id, d, order)
	if err = m.save(before); err != nil {
		return Connection{}, err
	}
	m.manualSuccess[id]++
	out := m.state.Snapshot.Connections[i]
	out.AccountIDs = append([]string{}, out.AccountIDs...)
	return out, nil
}
func (m *Manager) markFailed(id string, err error) {
	i, _ := m.connection(id)
	c := &m.state.Snapshot.Connections[i]
	c.Status = "error"
	if err.Error() == "session_expired" || err.Error() == "permission_denied" {
		c.Status = "reconnect_required"
	}
	c.Error = err.Error()
	c.UpdatedAt = time.Now().UTC()
	for i := range m.state.Snapshot.Accounts {
		a := &m.state.Snapshot.Accounts[i]
		if a.ConnectionID == id {
			a.Status = "stale"
			a.Error = err.Error()
		}
	}
}
func (m *Manager) Refresh(ctx context.Context, id string) error {
	return m.refresh(ctx, id, false)
}

func (m *Manager) refresh(ctx context.Context, id string, background bool) error {
	m.mu.Lock()
	i, err := m.connection(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	c := m.state.Snapshot.Connections[i]
	if background && (m.active[id] > 0 || c.Status == "reconnect_required") {
		m.mu.Unlock()
		return errRefreshSkipped
	}
	session := m.state.Sessions[id]
	if session == "" {
		m.mu.Unlock()
		return failure("session_expired")
	}
	m.active[id]++
	defer m.finishOperation(id)
	m.generation[id]++
	generation := m.generation[id]
	m.order++
	order := m.order
	m.mu.Unlock()
	d, refreshErr := m.discover(ctx, c.Provider, session)
	if refreshErr == nil && !continuity(c, d) {
		refreshErr = failure("organization_mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err = m.connection(id); err != nil {
		return err
	}
	if m.generation[id] != generation {
		return failure("superseded")
	}
	if ctx.Err() != nil {
		return failure("provider_unavailable")
	}
	before := m.backup()
	if refreshErr != nil {
		m.markFailed(id, refreshErr)
	} else {
		m.publish(id, d, order)
	}
	if err = m.save(before); err != nil {
		return err
	}
	if !background && refreshErr == nil {
		m.manualSuccess[id]++
	}
	return refreshErr
}
func (m *Manager) DeleteConnection(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, err := m.connection(id)
	if err != nil {
		return err
	}
	before := m.backup()
	m.generation[id]++
	delete(m.state.Sessions, id)
	c := &m.state.Snapshot.Connections[i]
	c.Status = "reconnect_required"
	c.Error = ""
	c.UpdatedAt = time.Now().UTC()
	for i := range m.state.Snapshot.Accounts {
		a := &m.state.Snapshot.Accounts[i]
		if a.ConnectionID == id {
			a.Status = "stale"
			a.Error = ""
		}
	}
	return m.save(before)
}

// Bind validates account existence. The management caller validates credential eligibility and provider.
func (m *Manager) Bind(credentialID, accountID string, verified bool) error {
	return m.bind(credentialID, accountID, verified, "")
}

// BindVerified persists the caller's fingerprint of the verified credential context.
// Management must redact the fingerprint before serializing a public response.
func (m *Manager) BindVerified(credentialID, accountID, fingerprint string) error {
	if len(fingerprint) != 64 {
		return failure("invalid_binding")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return failure("invalid_binding")
	}
	return m.bind(credentialID, accountID, true, fingerprint)
}
func (m *Manager) bind(credentialID, accountID string, verified bool, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if credentialID == "" || len(credentialID) > 512 {
		return failure("invalid_binding")
	}
	found := false
	for _, a := range m.state.Snapshot.Accounts {
		if a.ID == accountID {
			found = true
		}
	}
	if !found {
		return failure("not_found")
	}
	before := m.backup()
	b := Binding{CredentialID: credentialID, AccountID: accountID, Verified: verified, VerificationFingerprint: fingerprint}
	found = false
	for i, existing := range m.state.Snapshot.Bindings {
		if existing.CredentialID == credentialID {
			m.state.Snapshot.Bindings[i] = b
			found = true
			break
		}
	}
	if !found {
		m.state.Snapshot.Bindings = append(m.state.Snapshot.Bindings, b)
	}
	return m.save(before)
}
func (m *Manager) Unbind(credentialID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	before := m.backup()
	out := make([]Binding, 0, len(m.state.Snapshot.Bindings))
	for _, b := range m.state.Snapshot.Bindings {
		if b.CredentialID != credentialID {
			out = append(out, b)
		}
	}
	m.state.Snapshot.Bindings = out
	return m.save(before)
}

func (m *Manager) finishOperation(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[id]--
	if m.active[id] == 0 {
		delete(m.active, id)
	}
}
