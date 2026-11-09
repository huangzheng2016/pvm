package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSyncDirDoesNotFailOnWindows verifies that syncDir doesn't cause
// "Access is denied" errors on Windows while still providing durability on Unix.
func TestSyncDirDoesNotFailOnWindows(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.img")

	// Create a test file
	if err := os.WriteFile(testFile, []byte("test"), 0o600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// This should not fail on any platform
	if err := syncDir(testFile); err != nil {
		t.Errorf("syncDir failed: %v", err)
	}
}
