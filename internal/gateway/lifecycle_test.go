package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentStopsWhileConfigUnavailable(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "invalid"}[invalid], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.json")
			if invalid {
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			a := New(path)
			done := make(chan error, 1)
			go func() { done <- a.Run() }()
			// Let Run enter its config retry path before requesting shutdown.
			time.Sleep(20 * time.Millisecond)
			close(a.stop)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("stop did not interrupt config retry")
			}
		})
	}
}
