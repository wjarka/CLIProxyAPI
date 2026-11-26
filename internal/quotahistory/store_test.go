package quotahistory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testObservation(at time.Time, remaining *float64) PoolObservation {
	return PoolObservation{AccountID: "account", Provider: "claude", PoolID: "weekly", Label: "Weekly", ObservedAt: at, ResetAt: at.Add(2 * time.Minute), RemainingPercent: remaining}
}
func number(n float64) *float64 { return &n }

func TestStoreRetainsLastObservationAcrossRestart(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := NewStore(path, 52*7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	old := testObservation(at, number(23))
	if err = s.Observe(old); err != nil {
		t.Fatal(err)
	}
	s, err = NewStore(path, 52*7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.ObservedAt = old.ResetAt.Add(time.Minute)
	next.ResetAt = old.ResetAt.Add(7 * 24 * time.Hour)
	next.RemainingPercent = number(100)
	if err = s.Observe(next); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot(at.Add(-time.Hour), next.ResetAt.Add(time.Hour))
	if len(snap.Cycles) != 2 {
		t.Fatalf("cycles: %+v", snap.Cycles)
	}
	if *snap.Cycles[0].RemainingPercent != 23 || snap.Cycles[0].State != "completed" {
		t.Fatalf("old: %+v", snap.Cycles[0])
	}
	if snap.Cycles[1].StartAt == nil || !snap.Cycles[1].StartAt.Equal(old.ResetAt) {
		t.Fatal("proven previous boundary lost")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestStoreUnknownIsNotZeroAndOlderReadingCannotReplace(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), 52*7*24*time.Hour)
	o := testObservation(at, number(0))
	if err := s.Observe(o); err != nil {
		t.Fatal(err)
	}
	older := o
	older.ObservedAt = at.Add(-time.Minute)
	older.RemainingPercent = number(70)
	_ = s.Observe(older)
	unknown := o
	unknown.ObservedAt = at.Add(time.Minute)
	unknown.RemainingPercent = nil
	_ = s.Observe(unknown)
	got := s.Snapshot(at.Add(-time.Hour), at.Add(24*time.Hour)).Cycles
	if len(got) != 1 || got[0].RemainingPercent == nil || *got[0].RemainingPercent != 0 || !got[0].ObservedAt.Equal(at) {
		t.Fatalf("%+v", got)
	}
	unknown.PoolID = "separate"
	_ = s.Observe(unknown)
	got = s.Snapshot(at.Add(-time.Hour), at.Add(24*time.Hour)).Cycles
	if len(got) != 2 || got[0].PoolID == got[1].PoolID {
		t.Fatal(got)
	}
}

func TestStoreResetCorrectionIsUncertain(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), 52*7*24*time.Hour)
	old := testObservation(at, number(23))
	_ = s.Observe(old)
	shifted := old
	shifted.ObservedAt = at.Add(time.Second)
	shifted.ResetAt = old.ResetAt.Add(time.Hour)
	shifted.RemainingPercent = number(40)
	_ = s.Observe(shifted)
	got := s.Snapshot(at.Add(-time.Hour), at.Add(24*time.Hour)).Cycles
	if len(got) != 2 || got[0].State != "uncertain" || *got[0].RemainingPercent != 23 || got[1].StartAt != nil {
		t.Fatal(got)
	}
}

func TestStoreRetentionAndBadInput(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), 52*7*24*time.Hour)
	ancient := testObservation(at.Add(-53*7*24*time.Hour), number(20))
	_ = s.Observe(ancient)
	for _, bad := range []PoolObservation{testObservation(at, number(-1)), testObservation(at, number(101)), {}} {
		if s.Observe(bad) == nil {
			t.Fatal("invalid observation accepted")
		}
	}
	got := s.Snapshot(at.Add(-54*7*24*time.Hour), at.Add(time.Hour))
	if len(got.Cycles) != 0 {
		t.Fatal(got)
	}
}

func TestStoreCorruptOversizedAndWriteFailure(t *testing.T) {
	for _, contents := range []string{"broken", strings.Repeat(" ", MaxArchiveBytes+1)} {
		path := filepath.Join(t.TempDir(), "h.json")
		_ = os.WriteFile(path, []byte(contents), 0600)
		s, err := NewStore(path, 52*7*24*time.Hour)
		if err == nil || s == nil {
			t.Fatal("expected recoverable read failure")
		}
		if s.Snapshot(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)).StorageStatus == "ready" {
			t.Fatal("failure hidden")
		}
	}
	s, _ := NewStore(filepath.Join(t.TempDir(), "missing", "h.json"), 52*7*24*time.Hour)
	s.path = filepath.Join(t.TempDir(), "is-directory")
	_ = os.Mkdir(s.path, 0700)
	if s.Observe(testObservation(time.Now(), number(23))) == nil {
		t.Fatal("write failure hidden")
	}
	if s.Snapshot(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)).StorageStatus != "degraded" {
		t.Fatal("missing degradation")
	}
}

func TestIdentityAndArchiveWhitelist(t *testing.T) {
	if Identity("claude", "account-1", "") != Identity("claude", "account-1", "") || Identity("claude", "account-1", "") == Identity("claude", "account-2", "") {
		t.Fatal("account identity")
	}
	if Identity("claude", "account-1", "") == Identity("codex", "account-1", "") || Identity("claude", "", "key-a") == Identity("claude", "", "key-b") {
		t.Fatal("identity isolation")
	}
	at := time.Now()
	s, _ := NewStore(filepath.Join(t.TempDir(), "h.json"), 52*7*24*time.Hour)
	_ = s.Observe(testObservation(at, number(23)))
	b, _ := json.Marshal(s.Snapshot(at.Add(-time.Hour), at.Add(time.Hour)))
	for _, secret := range []string{"access_token", "Authorization", "metadata", "dca_token"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("secret field", secret)
		}
	}
}
