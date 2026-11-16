package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"golang.org/x/term"
)

// The virtio-serial agent channel works even when guest networking is down.
func CmdExec(args []string) int {
	yes := false
	for len(args) > 0 && (args[0] == "--yes" || args[0] == "-y") {
		yes, args = true, args[1:]
	}
	if len(args) < 1 {
		return fatalf("usage: pvm exec [--yes] [<vm>] <cmd...>")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := ensureDaemon(p)
	if err != nil {
		return fatalf("%v", err)
	}
	explicit, command, err := splitVMArg(c, args)
	if err != nil {
		return fatalf("%v", err)
	}
	if len(command) == 0 {
		return fatalf("usage: pvm exec [--yes] [<vm>] <cmd...>")
	}
	// unlike ssh, exec does not enter the workspace folder: the agent runs
	// as root, which cannot enter the login user's reverse sshfs mounts
	name, _, _, err := resolveVM(c, p, explicit)
	if err != nil {
		return fatalf("%v", err)
	}
	if err := vmUp(c, name, yes); err != nil {
		return fatalf("%v", err)
	}
	return runExec(c, name, command)
}

// runExec runs argv in the VM over the agent channel.
func runExec(c *daemon.Client, name string, argv []string) int {
	body := map[string]any{"cmd": argv}
	if !isTTY(os.Stdin) {
		data, _ := io.ReadAll(os.Stdin)
		if len(data) > 0 {
			body["stdin_b64"] = base64.StdEncoding.EncodeToString(data)
		}
	}

	resp, err := c.PostStream(fmt.Sprintf("/v1/vms/%s/exec", name), body)
	if err != nil {
		return fatalf("exec: %v", err)
	}
	defer resp.Body.Close()

	exitCode := -1
	dec := json.NewDecoder(resp.Body)
	for {
		var f agent.Frame
		if err := dec.Decode(&f); err == io.EOF {
			break
		} else if err != nil {
			return fatalf("decode stream: %v", err)
		}
		switch {
		case f.Error != "":
			fmt.Fprintf(os.Stderr, "exec error: %s\n", f.Error)
			if exitCode < 0 {
				exitCode = 1
			}
		case f.Done:
			exitCode = f.ExitCode
		case f.Stream == agent.StreamStdout || f.Stream == agent.StreamStderr:
			data, err := base64.StdEncoding.DecodeString(f.DataB64)
			if err != nil {
				continue
			}
			if f.Stream == agent.StreamStdout {
				os.Stdout.Write(data)
			} else {
				os.Stderr.Write(data)
			}
		}
	}
	if exitCode < 0 {
		return 1
	}
	return exitCode
}

func CmdAgent(args []string) int {
	if runtime.GOOS != "linux" {
		return fatalf("pvm agent runs inside a linux guest")
	}
	fmt.Fprintln(os.Stderr, "pvm agent starting")
	if err := agent.NewServer().Run("/"); err != nil {
		return fatalf("%v", err)
	}
	return 0
}

func isTTY(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}
