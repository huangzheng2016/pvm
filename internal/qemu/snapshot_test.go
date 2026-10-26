package qemu

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/native"
)

func TestManifestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadManifest(dir); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("empty dir: want ErrNoSnapshot, got %v", err)
	}
	m := &SnapshotManifest{
		Name: "dev1", Arch: "arm64", CPUs: 2, MemMB: 2048,
		Accel: AccelHVF, PVMVersion: "test", CreatedAt: time.Now().Truncate(time.Second),
	}
	if err := m.save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != m.Name || got.Arch != m.Arch || got.CPUs != m.CPUs || got.MemMB != m.MemMB ||
		got.Accel != m.Accel || !got.CreatedAt.Equal(m.CreatedAt) || got.Consumed {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, m)
	}
	got.Consumed = true
	if err := got.save(dir); err != nil {
		t.Fatal(err)
	}
	again, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Consumed {
		t.Fatal("consumed flag lost")
	}
}

func TestCheckMatch(t *testing.T) {
	m := &SnapshotManifest{Arch: native.Arch, CPUs: 2, MemMB: 2048, Accel: AccelHVF, QEMUVersion: "11.1.2"}
	ok := &VMConfig{CPUs: 2, MemMB: 2048, Accel: AccelHVF}
	if err := m.checkMatch(ok, "11.1.2"); err != nil {
		t.Fatalf("identical config should match: %v", err)
	}
	cases := []VMConfig{
		{CPUs: 4, MemMB: 2048, Accel: AccelHVF},
		{CPUs: 2, MemMB: 4096, Accel: AccelHVF},
	}
	for i := range cases {
		if err := m.checkMatch(&cases[i], "11.1.2"); err == nil {
			t.Fatalf("case %d: expected mismatch error", i)
		}
	}
	m.Arch = "incompatible"
	if err := m.checkMatch(ok, "11.1.2"); err == nil {
		t.Fatal("incompatible snapshot architecture must be rejected")
	}
}

func TestCheckMatchAccelFallback(t *testing.T) {
	m := &SnapshotManifest{Arch: native.Arch, CPUs: 2, MemMB: 2048, Accel: AccelHVF, QEMUVersion: "11.1.2"}
	c := &VMConfig{CPUs: 2, MemMB: 2048, Accel: AccelTCG}
	if err := m.checkMatch(c, "11.1.2"); err != nil {
		t.Fatalf("hvf snapshot must be restorable on tcg host: %v", err)
	}
	if c.Accel != m.Accel {
		t.Fatalf("restore accel = %s, want snapshot accel %s", c.Accel, m.Accel)
	}
	m.Accel = AccelTCG
	c.Accel = AccelHVF
	if err := m.checkMatch(c, "11.1.2"); err == nil {
		t.Fatal("tcg snapshot on hvf host must remain incompatible")
	}
}

func TestCheckMatchGPUOverride(t *testing.T) {
	m := &SnapshotManifest{Arch: native.Arch, CPUs: 2, MemMB: 2048, Accel: AccelHVF, GPU: true, QEMUVersion: "11.1.2"}
	c := &VMConfig{CPUs: 2, MemMB: 2048, Accel: AccelHVF, GPU: true}
	if err := m.checkMatch(c, "11.1.2"); err != nil {
		t.Fatal(err)
	}
	m.GPU = false
	if err := m.checkMatch(c, "11.1.2"); err == nil {
		t.Fatal("gpu layout mismatch must refuse the heterogeneous restore")
	}
	m.GPU, m.Balloon = true, true
	if err := m.checkMatch(c, "11.1.2"); err == nil {
		t.Fatal("balloon layout mismatch must refuse the heterogeneous restore")
	}
	m.Balloon = false
	if err := m.checkMatch(c, "11.2.0"); err == nil {
		t.Fatal("qemu version drift must refuse the restore")
	}
}

func TestActiveDisk(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "disk.qcow2")
	if got := ActiveDisk(dir, def); got != def {
		t.Fatalf("no overlay: want default, got %s", got)
	}
	if err := os.WriteFile(SnapPaths(dir).Restore, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ActiveDisk(dir, def); got != SnapPaths(dir).Restore {
		t.Fatalf("overlay present: want restore overlay, got %s", got)
	}
}

func TestZstdPipeRoundtrip(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.Read(data[:1<<20]) // half random, half zeros — like RAM
	var buf bytes.Buffer
	if err := snapCompress(&buf, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if buf.Len() >= len(data) {
		t.Fatalf("expected compression to shrink half-zero input (%d >= %d)", buf.Len(), len(data))
	}
	var out bytes.Buffer
	if err := snapDecompress(&out, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("roundtrip data mismatch")
	}
}

func TestRemoveSnapshot(t *testing.T) {
	dir := t.TempDir()
	sp := SnapPaths(dir)
	for _, f := range []string{sp.RAM, sp.Snap, sp.Manifest} {
		os.WriteFile(f, []byte("x"), 0o600)
	}
	if err := RemoveSnapshot(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{sp.RAM, sp.Snap, sp.Manifest} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be gone", f)
		}
	}
	if err := RemoveSnapshot(dir); err != nil {
		t.Fatalf("removing twice should be a no-op: %v", err)
	}
}

func TestBrokenLiveRestore(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("HVF live-restore regression only exists on darwin/arm64")
	}
	tc := &Toolchain{Version: [3]int{11, 0, 0}}
	if !tc.BrokenLiveRestore(AccelHVF) {
		t.Fatal("qemu 11.0.0 + arm64 + hvf must be flagged broken")
	}
	if tc.BrokenLiveRestore(AccelKVM) {
		t.Fatal("non-hvf must not be flagged")
	}
	for _, v := range [][3]int{{10, 0, 3}, {11, 0, 1}, {11, 1, 0}, {11, 1, 2}, {12, 0, 0}} {
		tc.Version = v
		if tc.BrokenLiveRestore(AccelHVF) {
			t.Fatalf("qemu %v should not be flagged", v)
		}
	}
	// Bundled runtimes are pvm's own known-good builds: never flagged.
	tc = &Toolchain{Version: [3]int{11, 0, 0}, Bundled: true}
	if tc.BrokenLiveRestore(AccelHVF) {
		t.Fatal("bundled runtime must be exempt")
	}
}

func TestDetectBundledTrustsHVF(t *testing.T) {
	// A bundled toolchain is trusted without a trial boot: pointing it
	// at a nonexistent binary (probe would fail) must still yield hvf.
	tc := &Toolchain{System: "/nonexistent/qemu-system-aarch64", Bundled: true}
	if got := Detect(tc, "darwin"); got != AccelHVF {
		t.Fatalf("bundled darwin: want hvf without probe, got %s", got)
	}
	// Non-bundled with an unbootable binary still falls back to tcg.
	tc.Bundled = false
	if got := Detect(tc, "darwin"); got != AccelTCG {
		t.Fatalf("system qemu with dead binary: want tcg fallback, got %s", got)
	}
}

func TestRemoveSnapshotPreservesActiveDisk(t *testing.T) {
	dir := t.TempDir()
	sp := SnapPaths(dir)
	for _, path := range []string{sp.RAM, sp.Snap, sp.Restore, sp.Manifest} {
		if err := os.WriteFile(path, []byte("VM data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveSnapshot(dir); err == nil {
		t.Fatal("must refuse removal of active restore disk")
	}
	for _, path := range []string{sp.RAM, sp.Snap, sp.Restore, sp.Manifest} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "VM data" {
			t.Fatalf("snapshot artifact lost: %v", err)
		}
	}
}

func TestSnapshotReattachesSharesAfterPartialDetach(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "psnap")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "q.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	commands := make(chan map[string]any, 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, `{"QMP":{}}`)
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var cmd map[string]any
			if json.Unmarshal(line, &cmd) != nil {
				return
			}
			commands <- cmd
			switch cmd["execute"] {
			case "query-status":
				fmt.Fprintln(conn, `{"return":{"status":"running"}}`)
			case "device_del":
				id := cmd["arguments"].(map[string]any)["id"].(string)
				if id == "fs0" {
					fmt.Fprintln(conn, `{"return":{}}`)
					fmt.Fprintln(conn, `{"event":"DEVICE_DELETED","data":{"device":"fs0"}}`)
				} else {
					fmt.Fprintln(conn, `{"error":{"class":"DeviceNotFound","desc":"missing share"}}`)
				}
			default:
				fmt.Fprintln(conn, `{"return":{}}`)
			}
		}
	}()
	id, err := IdentityOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(RunState{PID: id.PID, Ident: id})
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "run.json")
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	in := &Instance{StatePath: statePath, Cfg: VMConfig{QMPSock: sock, ExtraMounts: []Mount{{Tag: "pvm0"}}}}
	if err := in.CreateSnapshot(dir, "test"); err == nil {
		t.Fatal("expected partial detach failure")
	}
	<-done
	close(commands)
	reattached := []string{}
	resumed := false
	for cmd := range commands {
		if cmd["execute"] == "cont" {
			resumed = true
		}
		if cmd["execute"] == "device_add" {
			reattached = append(reattached, cmd["arguments"].(map[string]any)["id"].(string))
		}
	}
	if !resumed {
		t.Fatal("failed snapshot did not resume guest")
	}
	if len(reattached) != 1 || reattached[0] != "fs0" {
		t.Fatalf("reattached devices = %v, want only fs0", reattached)
	}
}

func TestPrepareRestorePreservesExistingBackup(t *testing.T) {
	dir := t.TempDir()
	sp := SnapPaths(dir)
	tc := &Toolchain{Version: [3]int{11, 1, 2}, Bundled: true}
	cfg := VMConfig{CPUs: 2, MemMB: 2048, Accel: AccelTCG}
	m := &SnapshotManifest{Arch: native.Arch, CPUs: 2, MemMB: 2048, Accel: AccelTCG, QEMUVersion: tc.versionString()}
	if err := m.save(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sp.RAM, sp.Snap, sp.Restore} {
		if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bak := sp.Restore + ".bak"
	if err := os.WriteFile(bak, []byte("saved writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := &Instance{Cfg: cfg, Toolchain: tc}
	if _, err := in.PrepareRestore(dir); err == nil {
		t.Fatal("must refuse overwriting existing backup")
	}
	if data, err := os.ReadFile(bak); err != nil || string(data) != "saved writes" {
		t.Fatalf("backup lost: %q, %v", data, err)
	}
	if data, err := os.ReadFile(sp.Restore); err != nil || string(data) != "current" {
		t.Fatalf("current restore lost: %q, %v", data, err)
	}
}

func TestSnapshotRefusesExistingArtifacts(t *testing.T) {
	for _, artifact := range []string{"state.snapshot.zst", "disk.snap.qcow2", "disk.restore.qcow2", "snapshot.json"} {
		t.Run(artifact, func(t *testing.T) {
			dir := t.TempDir()
			id, err := IdentityOf(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(RunState{PID: id.PID, Ident: id})
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(dir, "run.json")
			if err := os.WriteFile(state, data, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, artifact)
			if err := os.WriteFile(path, []byte("saved state"), 0o600); err != nil {
				t.Fatal(err)
			}
			in := &Instance{StatePath: state}
			if err := in.CreateSnapshot(dir, "test"); err == nil || !strings.Contains(err.Error(), "snapshot") {
				t.Fatalf("existing snapshot accepted: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "saved state" {
				t.Fatalf("saved state changed: %q, %v", got, err)
			}
		})
	}
}
