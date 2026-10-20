package cli

import "testing"

func TestFormatDiskBytes(t *testing.T) {
	for n, want := range map[int64]string{
		512:         "512B",
		2048:        "2.0K",
		5 * 1 << 20: "5.0M",
		3 * 1 << 30: "3.0G",
		3 * 1 << 40: "3.0T",
	} {
		if got := formatDiskBytes(n); got != want {
			t.Errorf("formatDiskBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
