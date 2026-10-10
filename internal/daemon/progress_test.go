package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailLinesSmall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "console.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, size, err := tailLines(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
	if len(lines) != 2 || lines[0] != "line2" || lines[1] != "line3" {
		t.Errorf("lines = %v", lines)
	}
}

func TestTailLinesLargeDropsPartialFirst(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "console.log")
	// 300KB of lines; tail read window is 256KB so the first retained
	// line is partial and must be dropped.
	var sb strings.Builder
	for sb.Len() < 300*1024 {
		sb.WriteString("0123456789abcdef\n")
	}
	want := "0123456789abcdef"
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _, err := tailLines(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 10 {
		t.Fatalf("lines len = %d", len(lines))
	}
	for _, l := range lines {
		if l != want {
			t.Fatalf("partial or corrupt line retained: %q", l)
		}
	}
}

func TestTailLinesMissing(t *testing.T) {
	_, _, err := tailLines(filepath.Join(t.TempDir(), "nope.log"), 5)
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("want IsNotExist, got %v", err)
	}
}

func TestStageRingAndReady(t *testing.T) {
	s := &Server{prog: map[string][]StageEvent{}}
	for i := 0; i < 70; i++ {
		s.stage("vm1", "boot", "")
	}
	if got := len(s.stages("vm1")); got != 64 {
		t.Fatalf("ring len = %d, want 64", got)
	}
	if s.vmReady("vm1") {
		t.Fatal("no ready stage yet")
	}
	s.stage("vm1", "ready", "ssh reachable")
	if !s.vmReady("vm1") {
		t.Fatal("vmReady must see ready stage")
	}
	if s.vmReady("other") {
		t.Fatal("unknown vm must not be ready")
	}
}
