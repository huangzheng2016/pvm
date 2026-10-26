package qemu

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrokenLiveRestoreUnknownVersion(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("HVF live-restore regression only exists on darwin/arm64")
	}
	tc := &Toolchain{Version: [3]int{0, 0, 0}}
	if !tc.BrokenLiveRestore(AccelHVF) {
		t.Fatal("unparseable version must be treated as unknown and refused (fail closed)")
	}
	if tc.BrokenLiveRestore(AccelTCG) {
		t.Fatal("non-hvf must not be flagged")
	}
	tc.Bundled = true
	if tc.BrokenLiveRestore(AccelHVF) {
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

func TestDeviceDelReportsCommandError(t *testing.T) {
	sock, ln := fakeQMP(t, func(w *bufio.Writer, id string) {
		fmt.Fprintln(w, `{"error":{"class":"DeviceNotFound","desc":"Device missing"}}`)
	})
	defer ln.Close()
	q, err := DialQMP(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	start := time.Now()
	if err := q.DeviceDel("fs0", time.Second); err == nil || !strings.Contains(err.Error(), "DeviceNotFound") {
		t.Fatalf("DeviceDel error = %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("command error delayed until event timeout")
	}
}

func TestDeviceDelConsumesReplyAfterEvent(t *testing.T) {
	sock, ln := fakeQMP(t, func(w *bufio.Writer, id string) {
		fmt.Fprintln(w, `{"event":"DEVICE_DELETED","data":{"device":"`+id+`"}}`)
		fmt.Fprintln(w, `{"return":{}}`)
	})
	defer ln.Close()
	q, err := DialQMP(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.DeviceDel("fs0", time.Second); err != nil {
		t.Fatal(err)
	}
	if status, err := q.Status(); err != nil || status != "running" {
		t.Fatalf("next command reply corrupted: status=%q err=%v", status, err)
	}
}

func TestQMPConcurrentCommands(t *testing.T) {
	sock, ln := fakeQMP(t, func(w *bufio.Writer, id string) {})
	defer ln.Close()
	q, err := DialQMP(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status, err := q.Status(); err != nil || status != "running" {
				t.Errorf("Status() = %q, %v", status, err)
			}
		}()
	}
	wg.Wait()
}

func TestDeviceDelTimeoutIncludesCommandReply(t *testing.T) {
	sock, ln := fakeQMP(t, func(w *bufio.Writer, id string) {})
	defer ln.Close()
	q, err := DialQMP(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	start := time.Now()
	if err := q.DeviceDel("fs0", 30*time.Millisecond); err == nil {
		t.Fatal("expected command timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("device_del ignored timeout while waiting for command reply")
	}
}

func TestQMPTimeoutClosesConnection(t *testing.T) {
	sock, ln := fakeQMP(t, func(w *bufio.Writer, id string) {})
	defer ln.Close()
	q, err := DialQMP(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.DeviceDel("fs0", 10*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
	if _, err := q.Status(); err == nil {
		t.Fatal("timed out connection reused")
	}
}
