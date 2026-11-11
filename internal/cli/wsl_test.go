package cli

import (
	"reflect"
	"testing"
)

func TestSplitMountSpecWindowsAndUnix(t *testing.T) {
	cases := []struct {
		spec      string
		wantHost  string
		wantGuest string
		wantOK    bool
	}{
		{"/Users/dev/work:/home/dev/work", "/Users/dev/work", "/home/dev/work", true},
		{`C:\Users\dev\work:/home/dev/work`, `C:\Users\dev\work`, "/home/dev/work", true},
		{"D:/data/proj:/mnt/proj", "D:/data/proj", "/mnt/proj", true},
		{`C:\only-host`, "", "", false},
		{"invalid", "", "", false},
	}
	for _, tc := range cases {
		host, guest, ok := splitMountSpec(tc.spec)
		if ok != tc.wantOK || host != tc.wantHost || guest != tc.wantGuest {
			t.Fatalf("splitMountSpec(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.spec, host, guest, ok, tc.wantHost, tc.wantGuest, tc.wantOK)
		}
	}
}

func TestWindowsToWSLPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`C:\Users\dev\work`, "/mnt/c/Users/dev/work"},
		{`D:/data/set/`, "/mnt/d/data/set"},
		{`C:\`, "/mnt/c"},
		{"/home/dev/work", "/home/dev/work"},
	}
	for _, tc := range cases {
		if got := WindowsToWSLPath(tc.in); got != tc.want {
			t.Fatalf("WindowsToWSLPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRestoreGitBashPosixPath(t *testing.T) {
	exePath := `C:\Program Files\Git\bin`
	cases := []struct {
		in   string
		want string
	}{
		{`C:\Program Files\Git\home\dev\work`, "/home/dev/work"},
		{"C:/Program Files/Git/home/dev", "/home/dev"},
		{"c:/program files/git/home/dev", "/home/dev"},
		{"C:/Program Files/Git", "/"},
		{"C:/Program Files/Git2/home/dev", "C:/Program Files/Git2/home/dev"},
		{"C:/work/proj", "C:/work/proj"},
		{"/home/dev", "/home/dev"},
	}
	for _, tc := range cases {
		if got := restoreGitBashPosixPath(tc.in, exePath); got != tc.want {
			t.Errorf("restoreGitBashPosixPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := restoreGitBashPosixPath("C:/Program Files/Git/home/dev", ""); got != "C:/Program Files/Git/home/dev" {
		t.Errorf("path changed without Git Bash environment: %q", got)
	}
}

func TestRewriteArgsForWSL(t *testing.T) {
	in := []string{"mount", "dev1", "--profile", "readahead", `C:\Users\dev\proj:/home/dev/proj`}
	want := []string{"mount", "dev1", "--profile", "readahead", "/mnt/c/Users/dev/proj:/home/dev/proj"}
	if got := RewriteArgsForWSL(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("RewriteArgsForWSL(%v) = %v, want %v", in, got, want)
	}
	lsIn := []string{"mount", "ls", "dev1"}
	if got := RewriteArgsForWSL(lsIn); !reflect.DeepEqual(got, lsIn) {
		t.Fatalf("RewriteArgsForWSL(ls) mutated: %v", got)
	}
	t.Setenv("EXEPATH", `C:\Program Files\Git\bin`)
	rmIn := []string{"mount", "rm", "dev1", "C:/Program Files/Git/home/dev/proj"}
	rmWant := []string{"mount", "rm", "dev1", "/home/dev/proj"}
	if got := RewriteArgsForWSL(rmIn); !reflect.DeepEqual(got, rmWant) {
		t.Fatalf("RewriteArgsForWSL(rm) = %v, want %v", got, rmWant)
	}
}
