package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pvm")
	if err := os.WriteFile(path, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement = %q, err = %v", data, err)
	}
}
