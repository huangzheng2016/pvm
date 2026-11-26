//go:build windows

package qemu

import "testing"

func TestWHPXQEMU64Model(t *testing.T) {
	for bits, want := range map[uint64]string{
		0:    "qemu64",
		0x60: "qemu64",
		0x0c: "qemu64,+ssse3,+sse4.1",
		0x9c: "qemu64,+ssse3,+sse4.1,+sse4.2,+popcnt",
	} {
		if got := whpxQEMU64Model(bits); got != want {
			t.Errorf("bits %#x: got %q, want %q", bits, got, want)
		}
	}
}
