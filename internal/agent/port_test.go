package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindVirtioPort(t *testing.T) {
	root := t.TempDir()
	vp := filepath.Join(root, "sys", "class", "virtio-ports", "vport0p3")
	if err := os.MkdirAll(vp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vp, "name"), []byte("pvm.agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(root, "sys", "class", "virtio-ports", "vport0p2")
	os.MkdirAll(decoy, 0o755)
	os.WriteFile(filepath.Join(decoy, "name"), []byte("org.qemu.guest_agent.0\n"), 0o644)

	devDir := filepath.Join(root, "dev")
	os.MkdirAll(devDir, 0o755)
	os.WriteFile(filepath.Join(devDir, "vport0p3"), nil, 0o644)

	dev, err := FindVirtioPort(root, "pvm.agent")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "dev", "vport0p3")
	if dev != want {
		t.Fatalf("got %s want %s", dev, want)
	}

	if _, err := FindVirtioPort(root, "no.such.port"); err == nil {
		t.Fatal("want error for missing port name")
	}
}
