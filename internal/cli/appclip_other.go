//go:build !windows && !darwin && !linux

package cli

import "fmt"

const clipboardBridgeSupported = false

func runClipboardBridge(string, int, string, clipBridge) error {
	return fmt.Errorf("the clipboard bridge is not available on this host")
}
