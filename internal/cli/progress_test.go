package cli

import (
	"encoding/json"
	"fmt"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

func TestFollowProvisionReportsInstallationFailure(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pvm-progress-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := paths.New(dir)
	if err := os.MkdirAll(filepath.Dir(p.DaemonSock()), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.DaemonSock())
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms/dev1/console" {
			json.NewEncoder(w).Encode(map[string]any{"size": 0, "lines": []string{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"spec": map[string]any{"name": "dev1"}, "running": true, "ready": false,
			"stages": []map[string]string{{"stage": "ssh", "detail": "SSH reachable"}, {"stage": "failed", "detail": "agent installation failed; retry pvm start dev1"}},
		})
	})
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if code := followProvision(daemon.NewClient(p), "dev1", time.Second); code != 1 {
		t.Fatalf("installation failure returned %d", code)
	}
}

func TestVMState(t *testing.T) {
	view := func(js string) vmInfoView {
		var in vmInfoView
		if err := json.Unmarshal([]byte(js), &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	for _, tc := range []struct {
		js, state, hint string
	}{
		{`{"spec":{"role":"user"},"ready":true,"stage":{"stage":"ready"}}`, "stopped", ""},
		{`{"spec":{"role":"user"},"stage":{"stage":"failed","detail":"retry"}}`, "stopped", ""},
		{`{"spec":{"role":"user"},"running":true,"ready":true,"stage":{"stage":"ready"}}`, "ready", ""},
		{`{"spec":{"role":"user"},"running":true,"stage":{"stage":"failed","detail":"retry"}}`, "failed", "retry"},
		{`{"spec":{"role":"user"},"running":true,"stage":{"stage":"provision","detail":"waiting for ssh"}}`, "starting (provision)", ""},
		{`{"spec":{"role":"user"},"running":true,"stage":{"stage":"disk+seed"}}`, "starting (disk+seed)", ""},
		{`{"spec":{"role":"user"},"running":true}`, "starting", ""},
		{`{"spec":{"role":"user","identity_reset":true},"running":true,"stage":{"stage":"ready"}}`, "starting", ""},
		{`{"spec":{"role":"gateway"},"running":true}`, "running", ""},
		{`{"spec":{"role":"gateway"}}`, "stopped", ""},
	} {
		if state, hint := vmState(view(tc.js)); state != tc.state || hint != tc.hint {
			t.Errorf("vmState(%s) = %q, %q; want %q, %q", tc.js, state, hint, tc.state, tc.hint)
		}
	}
}

func TestListShowsReadiness(t *testing.T) {
	requests := 0
	fakeVMDaemonWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/vms" {
			return false
		}
		requests++
		fmt.Fprint(w, `[{"running":true,"spec":{"image":"ubuntu:24.04","name":"pvm-gw1","role":"gateway"}},`+
			`{"running":true,"ready":true,"spec":{"image":"ubuntu:24.04","name":"dev1","role":"user"},"stage":{"stage":"ready"}},`+
			`{"running":true,"spec":{"image":"ubuntu:24.04","name":"dev2","role":"user"},"stage":{"stage":"ssh"}},`+
			`{"running":true,"spec":{"image":"ubuntu:24.04","name":"dev3","role":"user"},"stage":{"stage":"failed","detail":"retry with pvm start dev3"}},`+
			`{"spec":{"image":"ubuntu:24.04","name":"dev4","role":"user"}}]`)
		return true
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code, out := captureStdout(t, func() int { return CmdList(nil) })
	os.Stderr = old
	w.Close()
	errOut, _ := io.ReadAll(r)
	r.Close()
	if code != 0 {
		t.Fatalf("pvm list: exit %d: %s", code, errOut)
	}
	stderr := string(errOut)
	want := map[string]string{"pvm-gw1": "running", "dev1": "ready", "dev2": "starting (ssh)", "dev3": "failed", "dev4": "stopped"}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		cols := regexp.MustCompile(`\s{2,}`).Split(line, -1)
		if len(cols) < 4 || cols[3] != want[cols[0]] {
			t.Errorf("row %q: want status %q", line, want[cols[0]])
		}
		delete(want, cols[0])
	}
	if len(want) != 0 {
		t.Errorf("rows missing: %v\n%s", want, out)
	}
	if stderr != "pvm: dev3 failed: retry with pvm start dev3\n" {
		t.Errorf("stderr = %q", stderr)
	}
	if requests != 1 {
		t.Errorf("list made %d requests", requests)
	}
}
