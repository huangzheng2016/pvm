package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

var jsonOutput bool

func jsonFlag(fs *flag.FlagSet) {
	fs.BoolVar(&jsonOutput, "json", false, "output machine-readable JSON")
}

// scanJSONFlag strips --json before `--` for commands that parse positionals
// by hand, leaving guest-bound arguments after `--` untouched.
func scanJSONFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if a == "--json" {
			jsonOutput = true
			continue
		}
		out = append(out, a)
	}
	return out
}

func writeJSONOut(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// progressWriter keeps human progress off stdout when --json is active.
func progressWriter() io.Writer {
	if jsonOutput {
		return os.Stderr
	}
	return os.Stdout
}

func findAPIError(args ...any) *daemon.APIError {
	for _, a := range args {
		if err, ok := a.(error); ok {
			var ae *daemon.APIError
			if errors.As(err, &ae) {
				return ae
			}
		}
	}
	return nil
}

func exitCodeForError(err error) int {
	var ae *daemon.APIError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "wait_timeout", "timeout":
			return 4
		case "invalid_input":
			return 2
		}
	}
	return 1
}

func failErr(err error) int {
	if err == nil {
		return 0
	}
	code := exitCodeForError(err)
	if jsonOutput {
		var ae *daemon.APIError
		if errors.As(err, &ae) {
			_ = writeJSONOut(map[string]any{
				"status": "error",
				"error": map[string]any{
					"code":        ae.Code,
					"message":     ae.Message,
					"next_action": ae.NextAction,
					"retryable":   ae.Retryable,
				},
			})
			return code
		}
		_ = writeJSONOut(map[string]any{
			"status": "error",
			"error":  map[string]any{"code": "cli_error", "message": err.Error()},
		})
		return code
	}
	var ae *daemon.APIError
	if errors.As(err, &ae) && ae.NextAction != "" && !strings.Contains(ae.Message, ae.NextAction) {
		fmt.Fprintf(os.Stderr, "pvm: %s (next: %s)\n", ae.Message, ae.NextAction)
		return code
	}
	fmt.Fprintf(os.Stderr, "pvm: %s\n", err)
	return code
}
