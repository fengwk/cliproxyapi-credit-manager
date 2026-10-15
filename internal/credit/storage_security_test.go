package credit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A lost pepper must not be silently replaced while an existing ledger survives.
func TestMissingPepperFailsClosedWithoutChangingLedger(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateKey(t.Context(), KeySpec{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	pepper := filepath.Join(dir, pepperFileName)
	if err := os.Remove(pepper); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(Options{DataDir: dir})
		if s != nil || !errors.Is(err, ErrInvalid) {
			t.Fatalf("lost pepper accepted: store=%v err=%v", s, err)
		}
		if _, err := os.Stat(pepper); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pepper was regenerated: %v", err)
		}
	}
}

// Every financial file remains private, including WAL/SHM created by SQLite.
func TestLedgerFilePermissions(t *testing.T) {
	s, err := Open(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, path := range []string{s.dbPath, s.walPath, s.dbPath + "-shm", filepath.Join(filepath.Dir(s.dbPath), pepperFileName)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("file readable by other users: %s %v", filepath.Base(path), info.Mode())
		}
	}
}
