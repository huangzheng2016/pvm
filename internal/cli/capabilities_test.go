package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCapabilitiesIsOfflineAndValid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("PVM_HOME", dir)
	rc, out := captureStdout(t, func() int { return CmdCapabilities(nil) })
	if rc != 0 {
		t.Fatalf("capabilities rc = %d", rc)
	}
	var doc struct {
		Protocol     string         `json:"protocol"`
		JSONCommands []string       `json:"json_commands"`
		ExitCodes    map[string]int `json:"exit_codes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("capabilities output is not JSON: %v\n%s", err, out)
	}
	if doc.Protocol != "pvm-agent/1" || len(doc.JSONCommands) == 0 || doc.ExitCodes["wait_timeout"] != 4 {
		t.Fatalf("unexpected capabilities: %+v", doc)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("capabilities must not create PVM_HOME: %v", err)
	}
}
