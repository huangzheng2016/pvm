package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestParseMountAddArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantProfile string
	}{
		{"default", []string{"dev1", "/host:/guest"}, vm.MountProfileCoherent},
		{"coherent", []string{"dev1", "--profile", "coherent", "/host:/guest"}, vm.MountProfileCoherent},
		{"readahead", []string{"dev1", "--profile", "readahead", "/host:/guest"}, vm.MountProfileReadahead},
		{"equals", []string{"dev1", "--profile=readahead", "/host:/guest"}, vm.MountProfileReadahead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMountAddArgs(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.vmName != "dev1" || got.host != "/host" || got.guest != "/guest" || got.profile != tc.wantProfile {
				t.Fatalf("parsed mount = %+v", got)
			}
		})
	}
}

func TestParseMountAddArgsRejectsInvalidInput(t *testing.T) {
	tests := [][]string{
		{"dev1", "--profile", "loose", "/host:/guest"},
		{"dev1", "--profile", "fscache", "/host:/guest"},
		{"dev1", "--profile", "readahead;id", "/host:/guest"},
		{"dev1", "--options", "cache=loose", "/host:/guest"},
		{"dev1", "--profile", "/host:/guest"},
		{"dev1", "--profile", "readahead"},
		{"dev1", "not-a-mount"},
		{"dev1", "/host:/guest", "extra"},
	}
	for _, args := range tests {
		if _, err := parseMountAddArgs(args); err == nil {
			t.Errorf("parseMountAddArgs(%q) unexpectedly passed", args)
		}
	}
}

func TestWriteMountTable(t *testing.T) {
	var out bytes.Buffer
	mounts := []mountView{
		{Host: "/host/a", Guest: "/mnt/legacy"},
		{Host: "/host/b", Guest: "/mnt/read", Profile: vm.MountProfileReadahead},
	}
	if err := writeMountTable(&out, mounts); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("mount table = %q", out.String())
	}
	want := [][]string{
		{"HOST", "GUEST", "PROFILE"},
		{"/host/a", "/mnt/legacy", "coherent"},
		{"/host/b", "/mnt/read", "readahead"},
	}
	for i := range want {
		got := strings.Fields(lines[i])
		if strings.Join(got, " ") != strings.Join(want[i], " ") {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
	if err := writeMountTable(&out, []mountView{{Profile: "loose"}}); err == nil {
		t.Fatal("invalid stored profile must be visible as an error")
	}
}
