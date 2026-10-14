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
