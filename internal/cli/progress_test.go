package cli

import (
	"testing"
	"time"
)

func TestFilterConsoleLinesMarkers(t *testing.T) {
	lines := []string{
		"[    0.123456] cloud-init[567]: Cloud-init v. 24.1 running 'init' at Fri, 10 Oct 2026 08:00:00 +0000. Up 12.3 seconds.",
		"[    0.200000] randomkernel: some unrelated boot noise",
		"Get:1 http://archive.ubuntu.com/ubuntu noble InRelease [265 kB]",
		"Fetched 25.4 MB in 3s (8,123 kB/s)",
		"[   40.000000] cloud-init[567]: Cloud-init v. 24.1 running 'modules:final' at Fri, 10 Oct 2026 08:01:00 +0000",
		"Cloud-init v. 24.1 finished at Fri, 10 Oct 2026 08:01:30 +0000. Datasource DataSourceNoCloud [seed=/dev/vdb]. Up 90.12 seconds",
		"Something failed miserably",
		"",
	}
	got := filterConsoleLines(lines, false)
	joined := ""
	for _, l := range got {
		joined += l + "\n"
	}
	for _, want := range []string{"Cloud-init v. 24.1 running 'init'", "Fetched 25.4 MB", "modules:final", "finished", "failed miserably"} {
		if !contains(got, want) {
			t.Errorf("expected match for %q in:\n%s", want, joined)
		}
	}
	if contains(got, "unrelated boot noise") {
		t.Errorf("noise line should be filtered out")
	}
}

func TestFilterConsoleLinesRaw(t *testing.T) {
	lines := []string{"a", "b", "c"}
	if got := filterConsoleLines(lines, true); len(got) != 3 {
		t.Errorf("raw mode must pass all lines, got %v", got)
	}
}

func TestStripKernelPrefix(t *testing.T) {
	cases := map[string]string{
		"[   12.345678] cloud-init[567]: Cloud-init v. 24.1 running 'init'": "cloud-init: Cloud-init v. 24.1 running 'init'",
		"plain line no prefix":  "plain line no prefix",
		"[0.1] something: text": "something: text",
	}
	for in, want := range cases {
		if got := stripKernelPrefix(in); got != want {
			t.Errorf("stripKernelPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStuckTracker(t *testing.T) {
	tr := newStuckTracker(50 * time.Millisecond)
	tr.observe(100)
	if tr.observe(100) {
		t.Fatal("should not report stuck immediately")
	}
	tr.observe(200)
	time.Sleep(60 * time.Millisecond)
	if !tr.observe(200) {
		t.Fatal("should report stuck after silence")
	}
	if tr.observe(300) {
		t.Fatal("new output must clear stuck state")
	}
}

func TestSkipOverlap(t *testing.T) {
	prev := []string{"a", "b", "c", "d"}
	cur := []string{"c", "d", "e", "f"}
	got := skipOverlap(prev, cur)
	if len(got) != 2 || got[0] != "e" || got[1] != "f" {
		t.Errorf("skipOverlap = %v, want [e f]", got)
	}
	// Repeated verbatim lines must not confuse the alignment.
	prev = []string{"x", "x", "x"}
	cur = []string{"x", "x", "x", "x"}
	got = skipOverlap(prev, cur)
	if len(got) != 1 || got[0] != "x" {
		t.Errorf("skipOverlap repeat = %v, want [x]", got)
	}
	got = skipOverlap([]string{"a"}, []string{"b", "c"})
	if len(got) != 2 {
		t.Errorf("skipOverlap disjoint = %v, want [b c]", got)
	}
}

func contains(lines []string, sub string) bool {
	for _, l := range lines {
		if len(sub) <= len(l) {
			for i := 0; i+len(sub) <= len(l); i++ {
				if l[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
