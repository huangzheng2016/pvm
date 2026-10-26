package cli

import (
	"github.com/huangzheng2016/pvm/internal/paths"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSSHWantTTY(t *testing.T) {
	if !sshWantsTTY(nil, false, false) {
		t.Fatal("interactive login must allocate a TTY")
	}
	if !sshWantsTTY([]string{"codex"}, true, true) {
		t.Fatal("interactive command from a terminal must allocate a TTY")
	}
	if sshWantsTTY([]string{"codex"}, true, false) {
		t.Fatal("piped stdout must not allocate a TTY")
	}
	if sshWantsTTY([]string{"codex"}, false, true) {
		t.Fatal("piped stdin must not allocate a TTY")
	}
}

func TestSSHLoginCommand(t *testing.T) {
	for _, tc := range []struct {
		tty  bool
		want string
	}{
		{false, `exec "${SHELL:-/bin/sh}" -lc 'claude --version'`},
		{true, `exec "${SHELL:-/bin/sh}" -lic 'claude --version'`},
	} {
		if got := sshLoginCommand([]string{"claude", "--version"}, tc.tty); got != tc.want {
			t.Errorf("sshLoginCommand(tty=%v) = %q, want %q", tc.tty, got, tc.want)
		}
	}
}

func TestSSHLoginCommandQuotes(t *testing.T) {
	got := sshLoginCommand([]string{`printf '%s' "a b"`}, false)
	want := `exec "${SHELL:-/bin/sh}" -lc 'printf '"'"'%s'"'"' "a b"'`
	if got != want {
		t.Fatalf("quoted command = %q, want %q", got, want)
	}
}

func TestSSHStreamsStderrAndPreservesAgentFallback(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "pvm-ssh-")
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := home
	t.Cleanup(func() { os.RemoveAll(stateRoot) })
	home = filepath.Join(home, "state ' $(touch sentinel)")
	t.Setenv("PVM_HOME", home)
	p := paths.New(home)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.DaemonSock())
	if err != nil {
		t.Fatal(err)
	}
	var execCalls atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			io.WriteString(w, `{"ready":true}`)
		case "/v1/vms/dev1":
			io.WriteString(w, `{"running":true,"gw_ssh_port":2222,"gw_user":"baker","spec":{"name":"dev1","user":"baker","ip":"203.0.113.7"}}`)
		case "/v1/vms/dev1/exec":
			execCalls.Add(1)
			io.WriteString(w, "{\"done\":true,\"exit_code\":0}\n")
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	argvPath := filepath.Join(bin, "ssh-argv")
	t.Setenv("PVM_TEST_SSH_ARGV", argvPath)
	for _, tc := range []struct {
		name, script, wantOut, wantErr string
		code, agentCalls               int
	}{
		{"success", "printf 'remote stdout\\n'; printf 'remote stderr\\n' >&2", "remote stdout\n", "remote stderr\n", 0, 0},
		{"command failure", "printf 'command failed\\n' >&2; exit 7", "", "command failed\n", 7, 0},
		{"connection failure", "printf 'connection refused\\n' >&2; exit 255", "", "connection refused\nssh unreachable (connection refused), using agent channel\n", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PVM_TEST_SSH_ARGV\"\n"+tc.script+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			stdout, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			stderr, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			stdin, err := os.CreateTemp(t.TempDir(), "stdin")
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			beforeOut, beforeErr, beforeIn := os.Stdout, os.Stderr, os.Stdin
			os.Stdout, os.Stderr, os.Stdin = stdout, stderr, stdin
			defer func() { os.Stdout, os.Stderr, os.Stdin = beforeOut, beforeErr, beforeIn }()
			if code := CmdSSH([]string{"-T", "dev1", "true"}); code != tc.code {
				t.Fatalf("exit = %d, want %d", code, tc.code)
			}
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			arguments := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
			joined := strings.Join(arguments, " ")
			for _, flag := range []string{"-F /dev/null", "-o IdentitiesOnly=yes", "-o IdentityAgent=none", "-o ForwardAgent=no"} {
				if strings.Count(joined, flag) != 2 {
					t.Fatalf("outer and proxy SSH must isolate credentials: missing %q in %q", flag, arguments)
				}
			}
			var proxy string
			for _, arg := range arguments {
				if strings.HasPrefix(arg, "ProxyCommand=") {
					proxy = strings.TrimPrefix(arg, "ProxyCommand=")
				}
			}
			if proxy == "" {
				t.Fatal("missing proxy command")
			}
			// Execute only the proxy shell syntax against our fake ssh.
			proxyCapture := filepath.Join(bin, "proxy-argv")
			sh := exec.Command("/bin/sh", "-c", proxy)
			sh.Dir = bin
			sh.Env = append(os.Environ(), "PVM_TEST_SSH_ARGV="+proxyCapture)
			sh.Run()
			proxyArgv, err := os.ReadFile(proxyCapture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(proxyArgv), "-i\n"+p.SSHKey()+"\n") {
				t.Fatalf("proxy split or expanded key path: %q", proxyArgv)
			}
			if _, err := os.Stat(filepath.Join(bin, "sentinel")); !os.IsNotExist(err) {
				t.Fatal("proxy command executed key-path substitution")
			}
			if strings.Contains(joined, "-tt") {
				t.Fatal("-T unexpectedly allocated a TTY")
			}
			out, err := os.ReadFile(stdout.Name())
			if err != nil {
				t.Fatal(err)
			}
			errOut, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.wantOut || string(errOut) != tc.wantErr {
				t.Fatalf("stdout=%q stderr=%q", out, errOut)
			}
			if int(execCalls.Load()) != tc.agentCalls {
				t.Fatalf("agent calls=%d, want %d", execCalls.Load(), tc.agentCalls)
			}
		})
	}
}
