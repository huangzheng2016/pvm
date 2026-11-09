package cli

import (
	"strings"
	"testing"
)

func TestNativeShareMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		explicit  bool
		supports  bool
		goos      string
		wantMode  string
		wantWarn  bool
		wantErr   bool
	}{
		{name: "unix default", requested: "private", goos: "linux", wantMode: "private"},
		{name: "windows supported", requested: "private", supports: true, goos: "windows", wantMode: "private"},
		{name: "windows default fallback", requested: "private", goos: "windows", wantMode: "none", wantWarn: true},
		{name: "windows explicit share", requested: "private", explicit: true, goos: "windows", wantErr: true},
		{name: "windows explicit none", requested: "none", explicit: true, goos: "windows", wantMode: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, warning, err := nativeShareMode(tc.requested, tc.explicit, tc.supports, tc.goos)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if mode != tc.wantMode || (warning != "") != tc.wantWarn {
				t.Fatalf("mode=%q warning=%q, want mode=%q warn=%v", mode, warning, tc.wantMode, tc.wantWarn)
			}
		})
	}
	if !shareFlagExplicit([]string{"--share", "private"}) || !shareFlagExplicit([]string{"--share=none"}) ||
		!shareFlagExplicit([]string{"-share", "private"}) || !shareFlagExplicit([]string{"-share=none"}) {
		t.Fatal("explicit --share or -share not detected")
	}
	if shareFlagExplicit([]string{"--image", "ubuntu:24.04"}) {
		t.Fatal("unrelated flag detected as --share")
	}
	if _, _, err := nativeShareMode("private", true, false, "windows"); err == nil || !strings.Contains(err.Error(), "PVM_BACKEND=wsl") {
		t.Fatalf("missing WSL guidance: %v", err)
	}
}
