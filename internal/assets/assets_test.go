package assets

import (
	"runtime"
	"testing"
)

func TestNativeAssetAllowed(t *testing.T) {
	for _, path := range []string{"agent-linux-" + runtime.GOARCH, "gateway-linux-" + runtime.GOARCH} {
		if !nativeAssetAllowed(path) {
			t.Errorf("native asset rejected: %s", path)
		}
	}
	other := "amd64"
	if runtime.GOARCH == other {
		other = "arm64"
	}
	for _, path := range []string{"agent-linux-" + other, "gateway-linux-" + other, "agent-linux-riscv64", "random.bin"} {
		if nativeAssetAllowed(path) {
			t.Errorf("non-native asset accepted: %s", path)
		}
	}
}
