//go:build !windows

package cli

import (
	"fmt"
	"io"
)

// The focus-gated clipboard bridge exists only for Windows hosts so far.
const clipboardBridgeSupported = false

type clipContent struct {
	kind string
	data []byte
}

func runClipboardBridge(string, int, string, func(clipContent) error) error {
	return fmt.Errorf("the clipboard bridge is not available on this host")
}

func sendClipboard(func(string) (io.ReadWriteCloser, error), clipContent) error {
	return fmt.Errorf("the clipboard bridge is not available on this host")
}
