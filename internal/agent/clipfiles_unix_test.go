//go:build !windows

package agent

import (
	"strings"
	"testing"
)

// Only local files count, and files that came from the host are not sent
// back.
func TestClipFilePaths(t *testing.T) {
	sel := "copy\nfile:///home/u/a%20b.txt\r\nfile:///home/u/%E4%B8%AD\nfile://otherhost/x\nhttps://example.com/y\nfile:///\nfile:///home/u/.cache/pvm/clipboard/abc/z\nfile:///home/u/../u/c\n"
	got := clipFilePaths(sel, "/home/u/.cache/pvm/clipboard")
	want := []string{"/home/u/a b.txt", "/home/u/中", "/home/u/c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}
