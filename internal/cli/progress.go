package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

// consoleView mirrors the daemon's console endpoint response.
type consoleView struct {
	Lines []string `json:"lines"`
	Size  int64    `json:"size"`
}

// interesting console lines during provisioning. cloud-init is chatty;
// follow mode shows only lines that answer "what is it doing now".
var cloudInitMarkers = []*regexp.Regexp{
	regexp.MustCompile(`Cloud-init v\.`),
	regexp.MustCompile(`running '(init|modules?)'`),
	regexp.MustCompile(`Cloud-init .* finished`),
	regexp.MustCompile(`^Get:\d+|^Fetched |^Selecting |^Setting up `),
	regexp.MustCompile(`Pulling fs layer|Download complete|Status: Downloaded|docker pull`),
	regexp.MustCompile(`(?i)\b(error|failed|failure|traceback)\b`),
	regexp.MustCompile(`SSH host keys|cidata|DataSourceNoCloud`),
}

// filterConsoleLines returns the subset of lines worth showing during
// create follow mode. When raw is true every line passes (pvm logs).
func filterConsoleLines(lines []string, raw bool) []string {
	if raw {
		return lines
	}
	var out []string
	for _, ln := range lines {
		plain := stripKernelPrefix(ln)
		for _, re := range cloudInitMarkers {
			if re.MatchString(plain) {
				out = append(out, plain)
				break
			}
		}
	}
	return out
}

var ansiEsc = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// stripKernelPrefix turns "[   12.345678] cloud-init[9]: msg" into
// "cloud-init: msg" for readable follow output; other lines pass through.
func stripKernelPrefix(ln string) string {
	ln = ansiEsc.ReplaceAllString(ln, "")
	if !strings.HasPrefix(ln, "[") {
		return strings.TrimSpace(ln)
	}
	end := strings.Index(ln, "]")
	if end < 0 || end+1 >= len(ln) {
		return strings.TrimSpace(ln)
	}
	rest := strings.TrimSpace(ln[end+1:])
	rest = strings.TrimPrefix(rest, "cloud-init: ")
	if i := strings.Index(rest, "]: "); i >= 0 && strings.HasPrefix(rest, "cloud-init[") {
		rest = "cloud-init: " + rest[i+3:]
	}
	return rest
}

// stuckTracker reports when the console has been silent too long.
type stuckTracker struct {
	after     time.Duration
	lastSize  int64
	lastNewAt time.Time
}

func newStuckTracker(after time.Duration) *stuckTracker {
	return &stuckTracker{after: after, lastNewAt: time.Now()}
}

// observe records the latest console size and reports whether the VM
// has produced no new output for the configured duration.
func (t *stuckTracker) observe(size int64) bool {
	if size != t.lastSize {
		t.lastSize = size
		t.lastNewAt = time.Now()
		return false
	}
	return time.Since(t.lastNewAt) > t.after
}

// followProvision streams a VM's provisioning progress until ssh is
// ready (daemon stage "ready"), the daemon reports a failure, or the
// timeout hits. It prints interesting cloud-init lines as they appear
// and a stuck hint when the console goes quiet.
func followProvision(c *daemon.Client, name string, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	stuck := newStuckTracker(90 * time.Second)
	stuckNoted := false
	printed := map[string]bool{}
	var seenSize int64 = -1
	start := time.Now()

	for time.Now().Before(deadline) {
		var cv consoleView
		if err := c.Get(fmt.Sprintf("/v1/vms/%s/console?tail=400", name), &cv); err == nil {
			if seenSize < 0 {
				seenSize = cv.Size // first poll: show only fresh lines
			}
			if cv.Size > seenSize {
				for _, ln := range filterConsoleLines(cv.Lines, false) {
					key := strings.TrimSpace(ln)
					if key == "" || printed[key] {
						continue
					}
					printed[key] = true
					fmt.Printf("        | %s\n", ln)
				}
				seenSize = cv.Size
			}
		}

		var info vmInfoView
		if err := c.Get("/v1/vms/"+name, &info); err == nil {
			for _, st := range info.Stages {
				key := st.Stage + st.Detail
				if printed[key] {
					continue
				}
				printed[key] = true
				switch st.Stage {
				case "ready":
					fmt.Printf("[5/5] provision: ssh ready (total %.0fs)\n", time.Since(start).Seconds())
					return 0
				case "provision", "agent":
					fmt.Printf("        .. %s %s\n", st.Stage, st.Detail)
				}
			}
		}

		if stuck.observe(cv.Size) && !stuckNoted {
			stuckNoted = true
			last := lastNonEmpty(cv.Lines, 3)
			fmt.Printf("        !? no console output for 90s; last lines:\n")
			for _, ln := range last {
				fmt.Printf("        !?   %s\n", ln)
			}
			fmt.Printf("        !? inspect: pvm logs %s --follow\n", name)
		}
		if !stuck.observe(cv.Size) {
			stuckNoted = false
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "provision still running after %v; keep watching: pvm logs %s --follow\n", timeout, name)
	return 1
}

func lastNonEmpty(lines []string, n int) []string {
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append([]string{lines[i]}, out...)
		}
	}
	return out
}

// CmdLogs implements `pvm logs <name> [--follow] [--tail N]`.
func CmdLogs(args []string) int {
	fs := newFlagSet("logs", "<name> [--follow] [--tail 200]")
	follow := fs.Bool("follow", false, "keep streaming new console lines")
	followShort := fs.Bool("f", false, "shorthand for --follow")
	tail := fs.Int("tail", 200, "lines to show initially")
	name := ""
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		// Go's flag package stops at the first positional; accept
		// `pvm logs <name> --follow` as well as flags-first.
		name, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if name == "" {
		name = fs.Arg(0)
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}

	var prev []string
	first := true
	for {
		var cv consoleView
		if err := c.Get(fmt.Sprintf("/v1/vms/%s/console?tail=%d", name, *tail), &cv); err != nil {
			return fatalf("console: %v", err)
		}
		fresh := cv.Lines
		if !first {
			fresh = skipOverlap(prev, cv.Lines)
		}
		for _, ln := range fresh {
			fmt.Println(ln)
		}
		prev = cv.Lines
		first = false
		if !*follow && !*followShort {
			return 0
		}
		time.Sleep(2 * time.Second)
	}
}

// skipOverlap drops the prefix of cur that repeats the suffix of prev
// (consecutive tail windows of a growing log overlap), so follow mode
// prints each line exactly once even when lines repeat verbatim.
func skipOverlap(prev, cur []string) []string {
	maxK := len(prev)
	if len(cur) < maxK {
		maxK = len(cur)
	}
	for k := maxK; k > 0; k-- {
		equal := true
		for i := 0; i < k; i++ {
			if prev[len(prev)-k+i] != cur[i] {
				equal = false
				break
			}
		}
		if equal {
			return cur[k:]
		}
	}
	return cur
}
