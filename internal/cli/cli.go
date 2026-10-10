// Package cli implements the pvm subcommands. Commands that need the
// control plane go through the daemon client; daemon management itself
// spawns or signals the daemon process.
package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/huangzheng2016/pvm/internal/paths"
)

func notImpl(name string) int {
	fmt.Printf("%s: not implemented yet (M2+)\n", name)
	return 0
}

func fatalf(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "pvm: "+format+"\n", args...)
	return 1
}

func mustPaths() (*paths.Paths, error) {
	p, err := paths.Default()
	if err != nil {
		return nil, err
	}
	return p, p.Ensure()
}

// newFlagSet with consistent usage output.
func newFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "usage: pvm %s %s\n", name, usage) }
	return fs
}
