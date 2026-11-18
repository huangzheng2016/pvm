//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pvm-desktop: pvm Desktop runs on Windows only")
	os.Exit(1)
}
