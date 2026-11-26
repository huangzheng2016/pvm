//go:build linux

package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const clipboardBridgeSupported = true

func linuxClipTools() (readText, writeText *exec.Cmd, ok bool) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-paste"); err == nil {
			if _, err := exec.LookPath("wl-copy"); err == nil {
				return exec.Command("wl-paste", "--no-newline"), exec.Command("wl-copy"), true
			}
		}
		return nil, nil, false
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return exec.Command("xclip", "-selection", "clipboard", "-o"), exec.Command("xclip", "-selection", "clipboard", "-i"), true
	}
	return nil, nil, false
}

func linuxVMFocused(vm string) bool {
	if _, err := exec.LookPath("xdotool"); err != nil {
		return false
	}
	out, err := exec.Command("xdotool", "getactivewindow", "getwindowname").Output()
	return err == nil && strings.HasSuffix(strings.TrimSpace(string(out)), " ["+vm+"]")
}

func runClipboardBridge(vm string, clientPID int, _ string, b clipBridge) error {
	_, _, ok := linuxClipTools()
	if !ok {
		return fmt.Errorf("install wl-clipboard (Wayland) or xclip (X11) for host-to-guest clipboard")
	}
	var focused bool
	var sent [32]byte
	for {
		if err := syscall.Kill(clientPID, 0); err != nil {
			return nil
		}
		nowFocused := linuxVMFocused(vm)
		if b.toGuest && nowFocused {
			var buf bytes.Buffer
			cmd := exec.Command("xclip", "-selection", "clipboard", "-o")
			if os.Getenv("WAYLAND_DISPLAY") != "" {
				cmd = exec.Command("wl-paste", "--no-newline")
			}
			cmd.Stdout = &buf
			if err := cmd.Run(); err == nil && buf.Len() > 0 && buf.Len() <= 1<<20 {
				c := clipContent{kind: "text", data: append([]byte(nil), buf.Bytes()...)}
				if sum := c.sum(); sum != sent {
					if err := b.send(c); err != nil {
						fmt.Fprintf(os.Stderr, "clipboard to %s: %v\n", vm, err)
					} else {
						sent = sum
					}
				}
			}
		}
		focused = nowFocused
		_ = focused
		time.Sleep(300 * time.Millisecond)
	}
}
