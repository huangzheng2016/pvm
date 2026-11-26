package hostnet

import (
	"runtime"
	"testing"
)

func TestVirtualIfaceFilter(t *testing.T) {
	for _, v := range []string{"utun0", "utun8", "bridge0", "vmnet1", "tap0", "tun0", "wg0", "awdl0", "llw0", "docker0", "veth123", "tailscale0", "lo0"} {
		if !IsVirtual(v) {
			t.Errorf("%s should be virtual", v)
		}
	}
	for _, p := range []string{"en0", "en3", "eth0", "enp0s3", "wlan0"} {
		if IsVirtual(p) {
			t.Errorf("%s should not be virtual", p)
		}
	}
}

func TestSelectPhysicalOnHost(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("container/CI linux hosts may only have virtual NICs")
	}
	ifc := SelectPhysical()
	if ifc == nil {
		t.Skip("no physical interface on this host")
	}
	if IsVirtual(ifc.Name) || ifc.Index <= 0 {
		t.Fatalf("unexpected physical interface: %+v", ifc)
	}
}

func TestDarwinParsers(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin parser fixture")
	}
}
