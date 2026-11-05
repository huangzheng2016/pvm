package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenFileDoesNotAcceptExposedOrAmbiguousInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("YOUR_TOKEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadToken("", path); err != nil || got != "YOUR_TOKEN" {
		t.Fatalf("token: %q, %v", got, err)
	}
	if _, err := loadToken("YOUR_TOKEN", path); err == nil {
		t.Fatal("accepted two sources")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken("", path); err == nil {
		t.Fatal("accepted exposed token file")
	}
}
