package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.json")
	if err := AtomicWrite(p, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("content: %s", data)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm: %o", fi.Mode().Perm())
	}
	if err := AtomicWrite(p, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestTryLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.lock")
	l1, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); err == nil {
		l1.Unlock()
		t.Fatal("second TryLock on held lock must fail")
	}
	l1.Unlock()
	l2, err := TryLock(p)
	if err != nil {
		t.Fatalf("TryLock after Unlock failed: %v", err)
	}
	l2.Unlock()
}
