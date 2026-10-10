package agent

import (
	"bytes"
	"fmt"
	"testing"
)

// TestExecNoOutputLoss is a regression test for the pipe-drain ordering
// race: Wait() used to run before the stdout/stderr drain completed,
// truncating output ~15% of the time for write-then-exit commands.
func TestExecNoOutputLoss(t *testing.T) {
	_, c := pipePair(t)
	for i := 0; i < 100; i++ {
		var out bytes.Buffer
		want := fmt.Sprintf("line-%d", i)
		res, err := c.Exec([]string{"echo", want}, nil, &out, nil, 10)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("iter %d: exit %d", i, res.ExitCode)
		}
		if got := out.String(); got != want+"\n" {
			t.Fatalf("iter %d: got %q want %q (output truncated)", i, got, want+"\n")
		}
	}
}
