package cli

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

const jobUsage = "usage: pvm job <id> [--wait [duration]] [--json] | pvm job ls [--vm NAME] [--json]"

func CmdJob(args []string) int {
	args = scanJSONFlag(args)
	if len(args) == 0 {
		return failErr(fmt.Errorf("%s", jobUsage))
	}
	p, err := mustPaths()
	if err != nil {
		return failErr(err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return failErr(err)
	}
	if args[0] == "ls" {
		fs := flag.NewFlagSet("job ls", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		vmName := fs.String("vm", "", "filter by VM name")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		path := "/v1/jobs"
		if *vmName != "" {
			path += "?vm=" + url.QueryEscape(*vmName)
		}
		var jobs []daemon.Job
		if err := c.Get(path, &jobs); err != nil {
			return failErr(err)
		}
		if jsonOutput {
			if err := writeJSONOut(map[string]any{"status": "completed", "data": jobs}); err != nil {
				return failErr(err)
			}
			return 0
		}
		for _, j := range jobs {
			fmt.Printf("%s  %-9s %-10s %-16s %s %s\n", j.ID, j.Status, j.Kind, j.VM, j.Stage, j.Detail)
		}
		return 0
	}

	id := args[0]
	rest := args[1:]
	// Accept both `--wait` (default 10m) and `--wait 2m`.
	hasWait := false
	for i, a := range rest {
		if a == "--wait" || a == "-wait" {
			hasWait = true
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				rest[i] = "--wait=10m"
			}
		} else if strings.HasPrefix(a, "--wait=") || strings.HasPrefix(a, "-wait=") {
			hasWait = true
		}
	}
	fs := flag.NewFlagSet("job", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wait := fs.Duration("wait", 0, "wait up to this duration for completion")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
		return failErr(fmt.Errorf("%s", jobUsage))
	}

	deadline := time.Now().Add(*wait)
	for {
		remain := time.Until(deadline)
		waitSec := 0
		if hasWait && remain > 0 {
			waitSec = int(remain.Seconds()) + 1
			if waitSec > 30 {
				waitSec = 30
			}
		}
		var j daemon.Job
		err := c.Get(fmt.Sprintf("/v1/jobs/%s?wait=%d", url.PathEscape(id), waitSec), &j)
		if err != nil {
			if hasWait && time.Now().After(deadline) {
				return failErr(&daemon.APIError{
					Message:    fmt.Sprintf("job %s still running after %s", id, wait.String()),
					Code:       "wait_timeout",
					NextAction: "pvm job " + id + " --wait",
					Retryable:  true,
				})
			}
			return failErr(err)
		}
		if j.Status == daemon.JobRunning && hasWait && time.Now().Before(deadline) {
			continue
		}
		if jsonOutput {
			status := "completed"
			if j.Status == daemon.JobRunning {
				status = "running"
			} else if j.Status == daemon.JobFailed {
				status = "error"
			}
			if err := writeJSONOut(map[string]any{"status": status, "data": j}); err != nil {
				return failErr(err)
			}
		} else {
			fmt.Printf("job %s (%s %s): %s %s %s\n", j.ID, j.Kind, j.VM, j.Status, j.Stage, j.Detail)
			if j.Error != nil {
				fmt.Fprintf(os.Stderr, "error [%s]: %s\n", j.Error.Code, j.Error.Message)
			}
		}
		switch j.Status {
		case daemon.JobCompleted:
			return 0
		case daemon.JobFailed:
			return 1
		}
		return failErr(&daemon.APIError{
			Message:    fmt.Sprintf("job %s still running", id),
			Code:       "wait_timeout",
			NextAction: "pvm job " + id + " --wait",
			Retryable:  true,
		})
	}
}
