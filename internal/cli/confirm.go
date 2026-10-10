package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// stdinIsTTY reports whether the CLI can interactively prompt.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// confirmAction asks a [y/N] question; only an explicit y/yes confirms.
// Non-TTY input never confirms. IO is injectable for tests.
func confirmAction(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}
