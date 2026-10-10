package qemu

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrokenLiveRestoreUnknownVersion(t *testing.T) {
	tc := &Toolchain{Version: [3]int{0, 0, 0}}
	if !tc.BrokenLiveRestore("arm64", AccelHVF) {
		t.Fatal("unparseable version must be treated as unknown and refused (fail closed)")
	}
	if tc.BrokenLiveRestore("amd64", AccelHVF) || tc.BrokenLiveRestore("arm64", AccelTCG) {
		t.Fatal("non-arm64 or non-hvf must not be flagged")
	}
	tc.Bundled = true
	if tc.BrokenLiveRestore("arm64", AccelHVF) {
		t.Fatal("bundled runtime must be exempt")
	}
}

// fakeQMP is a scripted QMP server: greeting, capabilities, then per
// device_del it emits the scripted lines.
func fakeQMP(t *testing.T, onDeviceDel func(w *bufio.Writer, id string)) (string, net.Listener) {
	t.Helper()
	// unix socket path must stay short (macOS sun_path limit).
	dir, err := os.MkdirTemp("/tmp", "pqmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "q.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		w := bufio.NewWriter(conn)
		r := bufio.NewReader(conn)
		fmt.Fprintln(w, `{"QMP":{"version":{"qemu":{"major":11,"minor":1,"micro":2}},"capabilities":[]}}`)
		w.Flush()
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			s := string(line)
			switch {
			case strings.Contains(s, "qmp_capabilities"):
				fmt.Fprintln(w, `{"return":{}}`)
			case strings.Contains(s, "device_del"):
				id := "fs0"
				if strings.Contains(s, "fs9") {
					id = "fs9"
				}
				onDeviceDel(w, id)
			case strings.Contains(s, "query-status"):
				fmt.Fprintln(w, `{"event":"DEVICE_DELETED","data":{"device":"fs9"}}`)
				fmt.Fprintln(w, `{"return":{"status":"running"}}`)
			default:
				fmt.Fprintln(w, `{"return":{}}`)
			}
			w.Flush()
		}
	}()
	return sock, ln
}

func TestDeviceDelBuffersUnrelatedEvents(t *testing.T) {
	sock, ln := fakeQMP(t, func(w *bufio.Writer, id string) {
		fmt.Fprintln(w, `{"event":"SHUTDOWN"}`)
		fmt.Fprintln(w, `{"return":{}}`)
		fmt.Fprintln(w, `{"event":"DEVICE_DELETED","data":{"device":"`+id+`"}}`)
	})
	defer ln.Close()

	q, err := DialQMP(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	// Interleave a DEVICE_DELETED for fs9 into a normal Execute.
	if _, err := q.Execute("query-status", nil); err != nil {
		t.Fatal(err)
	}

	// DeviceDel(fs0): SHUTDOWN must end up buffered, not dropped.
	if err := q.DeviceDel("fs0", 5*time.Second); err != nil {
		t.Fatalf("DeviceDel fs0: %v", err)
	}
	if err := q.WaitEvent("SHUTDOWN", time.Second); err != nil {
		t.Fatalf("SHUTDOWN should have been buffered for WaitEvent: %v", err)
	}

	// DeviceDel(fs9) resolves from the buffer even though the server
	// only echoes a bare return for the command itself.
	if err := q.DeviceDel("fs9", 5*time.Second); err != nil {
		t.Fatalf("DeviceDel fs9 should match the buffered DEVICE_DELETED: %v", err)
	}
}
