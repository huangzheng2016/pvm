package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

func stdinIsTTY() bool {
	return isTTY(os.Stdin)
}

func confirmAction(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// confirmYes is confirmAction for questions whose safe answer is yes: a
// bare Enter accepts, and only an explicit no declines.
func confirmYes(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "%s [Y/n] ", prompt)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "n", "no":
		return false
	}
	return true
}
