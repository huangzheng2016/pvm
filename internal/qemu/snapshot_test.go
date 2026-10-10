package qemu

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	m := &SnapshotManifest{Arch: "arm64", CPUs: 2, MemMB: 2048, Accel: AccelHVF}
	ok := &VMConfig{Arch: "arm64", CPUs: 2, MemMB: 2048, Accel: AccelHVF}
	if err := m.checkMatch(ok); err != nil {
		t.Fatalf("identical config should match: %v", err)
	}
	cases := []VMConfig{
		{Arch: "amd64", CPUs: 2, MemMB: 2048, Accel: AccelHVF},
		{Arch: "arm64", CPUs: 4, MemMB: 2048, Accel: AccelHVF},
		{Arch: "arm64", CPUs: 2, MemMB: 4096, Accel: AccelHVF},
	}
	for i := range cases {
		if err := m.checkMatch(&cases[i]); err == nil {
			t.Fatalf("case %d: expected mismatch error", i)
		}
	}
}

func TestCheckMatchAccelFallback(t *testing.T) {
	m := &SnapshotManifest{Arch: "arm64", CPUs: 2, MemMB: 2048, Accel: AccelHVF}
	c := &VMConfig{Arch: "arm64", CPUs: 2, MemMB: 2048, Accel: AccelTCG}
	if err := m.checkMatch(c); err != nil {
		t.Fatalf("hvf snapshot must be restorable on tcg host: %v", err)
	}
	if c.Accel != m.Accel {
		t.Fatalf("restore accel = %s, want snapshot accel %s", c.Accel, m.Accel)
	}
	m.Accel = AccelTCG
	c.Accel = AccelHVF
	if err := m.checkMatch(c); err == nil {
		t.Fatal("tcg snapshot on hvf host must remain incompatible")
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
	for _, f := range []string{sp.RAM, sp.Snap, sp.Restore, sp.Manifest} {
		os.WriteFile(f, []byte("x"), 0o600)
	}
	if err := RemoveSnapshot(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{sp.RAM, sp.Snap, sp.Restore, sp.Manifest} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be gone", f)
		}
	}
	if err := RemoveSnapshot(dir); err != nil {
		t.Fatalf("removing twice should be a no-op: %v", err)
	}
}

func TestBrokenLiveRestore(t *testing.T) {
	tc := &Toolchain{Version: [3]int{11, 0, 0}}
	if !tc.BrokenLiveRestore("arm64", AccelHVF) {
		t.Fatal("qemu 11.0.0 + arm64 + hvf must be flagged broken")
	}
	if tc.BrokenLiveRestore("arm64", AccelKVM) || tc.BrokenLiveRestore("amd64", AccelHVF) {
		t.Fatal("non-hvf or non-arm64 must not be flagged")
	}
	for _, v := range [][3]int{{10, 0, 3}, {11, 0, 1}, {11, 1, 0}, {11, 1, 2}, {12, 0, 0}} {
		tc.Version = v
		if tc.BrokenLiveRestore("arm64", AccelHVF) {
			t.Fatalf("qemu %v should not be flagged", v)
		}
	}
	// Bundled runtimes are pvm's own known-good builds: never flagged.
	tc = &Toolchain{Version: [3]int{11, 0, 0}, Bundled: true}
	if tc.BrokenLiveRestore("arm64", AccelHVF) {
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
