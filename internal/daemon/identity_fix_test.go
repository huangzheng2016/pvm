package daemon

import "testing"

func TestSanitizeTZ(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"America/Los_Angeles\n", "America/Los_Angeles"},
		{"\n\nAsia/Shanghai\n", "Asia/Shanghai"},
		{"", "UTC"},
		{"   \n", "UTC"},
		{"$(rm -rf /)\n", "UTC"},
		{"America/Los_Angeles; reboot\n", "UTC"},
	} {
		if got := sanitizeTZ(tc.in); got != tc.want {
			t.Errorf("sanitizeTZ(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
