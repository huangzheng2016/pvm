package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/huangzheng2016/pvm/internal/exit"
)

// sshTarget is a deploy destination: [user@]host[:port].
type sshTarget struct {
	user string
	host string
	port string
}

func parseSSHTarget(s string) (sshTarget, error) {
	t := sshTarget{user: "root"}
	rest := s
	if u, h, ok := strings.Cut(s, "@"); ok {
		t.user = u
		rest = h
	}
	if h, p, ok := strings.Cut(rest, ":"); ok {
		t.host = h
		t.port = p
	} else {
		t.host = rest
	}
	if t.user == "" || t.host == "" {
		return t, fmt.Errorf("target must be [user@]host[:port], got %q", s)
	}
	return t, nil
}

func (t sshTarget) String() string { return t.user + "@" + t.host }

// deployHooks makes the deploy flow testable; production uses realExecHooks.
type deployHooks struct {
	ssh   func(t sshTarget, script string) (string, error)
	scp   func(t sshTarget, local, remote string) error
	build func(repoDir, goarch, out string) error
	probe func(server string) (string, error)
}

func realExecHooks() deployHooks {
	return deployHooks{
		ssh: func(t sshTarget, script string) (string, error) {
			args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}
			if t.port != "" {
				args = append(args, "-p", t.port)
			}
			args = append(args, t.String(), "bash", "-s")
			cmd := exec.Command("ssh", args...)
			cmd.Stdin = strings.NewReader(script)
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
		scp: func(t sshTarget, local, remote string) error {
			args := []string{"-o", "BatchMode=yes"}
			if t.port != "" {
				args = append(args, "-P", t.port)
			}
			args = append(args, local, t.String()+":"+remote)
			cmd := exec.Command("scp", args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("scp: %v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		build: func(repoDir, goarch, out string) error {
			cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/pvm")
			cmd.Dir = repoDir
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)
			if b, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("go build: %v: %s", err, strings.TrimSpace(string(b)))
			}
			return nil
		},
		probe: exit.ProbeFingerprint,
	}
}

// goarchFor maps `uname -m` to a supported GOARCH.
func goarchFor(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("unsupported remote architecture %q (pvm builds linux amd64/arm64)", machine)
}

// renderSystemdUnit renders the pvm-exit-agent unit.
func renderSystemdUnit(listen, token string) string {
	return fmt.Sprintf(`[Unit]
Description=pvm exit agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/pvm exit-agent --listen %s --token %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, listen, token)
}

// findRepoRoot walks up from dir looking for the pvm go.mod.
func findRepoRoot(dir string) (string, error) {
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/huangzheng2016/pvm") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("pvm repo not found from cwd (use --repo)")
		}
		dir = parent
	}
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// remoteDeployScript installs the new binary idempotently and reports the
// effective token; on health-check failure it restores pvm.prev and exits 1.
func remoteDeployScript(listen, unit, token string, port int) string {
	return fmt.Sprintf(`set -e
BIN=/usr/local/bin/pvm
NEWBIN=/usr/local/bin/pvm.new
UNIT=%s
LISTEN=%s
TOKEN=%s
mkdir -p "$(dirname "$BIN")"
if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
  pkill -f "pvm exit-agent" 2>/dev/null || true
  [ -f "$BIN" ] && cp -a "$BIN" "$BIN.prev" || true
  mv "$NEWBIN" "$BIN" && chmod +x "$BIN"
  nohup "$BIN" exit-agent --listen "$LISTEN" --token "$TOKEN" > /var/log/pvm-exit-agent.log 2>&1 &
  echo "MODE=nohup"
  echo "PVM_TOKEN=$TOKEN"
  exit 0
fi
if [ -f "$UNIT" ]; then
  EXISTING=$(sed -n 's/.*--token \([a-f0-9]*\).*/\1/p' "$UNIT" | head -1)
  if [ -n "$EXISTING" ]; then TOKEN="$EXISTING"; fi
else
  cat > "$UNIT" <<'UNITEOF'
%s
UNITEOF
  systemctl daemon-reload
fi
[ -f "$BIN" ] && cp -a "$BIN" "$BIN.prev" || true
mv "$NEWBIN" "$BIN" && chmod +x "$BIN"
systemctl enable --now pvm-exit-agent >/dev/null 2>&1 || true
systemctl restart pvm-exit-agent
ok=0
for i in $(seq 1 30); do
  if curl -ks "https://127.0.0.1:%d/v1/health" >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
if [ "$ok" != "1" ]; then
  echo "HEALTH_FAIL"
  if [ -f "$BIN.prev" ]; then
    mv "$BIN.prev" "$BIN"
    systemctl restart pvm-exit-agent || true
    echo "ROLLED_BACK"
  fi
  exit 1
fi
echo "MODE=systemd"
echo "PVM_TOKEN=$TOKEN"
`, unit, listen, token, renderSystemdUnit(listen, token), port)
}

// cmdExitDeploy implements `pvm exit deploy`.
func cmdExitDeploy(args []string) int {
	fs := newFlagSet("exit deploy", "root@<vps>[:port] [--name <exit>] [--listen :18443] [--repo <path>] [--samples N]")
	name := fs.String("name", "", "also register the exit locally with this name (auto speed probing)")
	listen := fs.String("listen", ":18443", "control plane listen address on the VPS")
	repo := fs.String("repo", "", "pvm repo path (default: detect from cwd)")
	samples := fs.Int("samples", 3, "speed samples per transport when --name is given")
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	if target == "" {
		fs.Usage()
		return 2
	}
	return runExitDeploy(realExecHooks(), target, *listen, *name, *repo, *samples, exitAdd)
}

func runExitDeploy(h deployHooks, targetArg, listen, name, repo string, samples int, addFn func([]string) int) int {
	t, err := parseSSHTarget(targetArg)
	if err != nil {
		return fatalf("%v", err)
	}
	if repo == "" {
		cwd, _ := os.Getwd()
		repo, err = findRepoRoot(cwd)
		if err != nil {
			return fatalf("%v", err)
		}
	}

	fmt.Printf("probing %s...\n", t)
	out, err := h.ssh(t, `uname -s; uname -m`)
	if err != nil {
		return fatalf("ssh %s: %v\n%s", t, err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "Linux" {
		return fatalf("remote is not Linux: %s", strings.TrimSpace(out))
	}
	goarch, err := goarchFor(lines[1])
	if err != nil {
		return fatalf("%v", err)
	}

	tmp, err := os.MkdirTemp("", "pvm-deploy-*")
	if err != nil {
		return fatalf("%v", err)
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "pvm")
	fmt.Printf("building linux/%s locally (VPS only receives the binary)...\n", goarch)
	if err := h.build(repo, goarch, bin); err != nil {
		return fatalf("%v", err)
	}

	token, err := newToken()
	if err != nil {
		return fatalf("%v", err)
	}
	if err := h.scp(t, bin, "/usr/local/bin/pvm.new"); err != nil {
		return fatalf("%v", err)
	}

	port := 18443
	if _, p, ok := strings.Cut(listen, ":"); ok && p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	out, err = h.ssh(t, remoteDeployScript(listen, "/etc/systemd/system/pvm-exit-agent.service", token, port))
	fmt.Print(out)
	if err != nil {
		return fatalf("deploy failed on %s: %v", t, err)
	}
	effToken := token
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "PVM_TOKEN="); ok && v != "" {
			effToken = v
		}
	}

	fp, err := h.probe(fmt.Sprintf("%s:%d", t.host, port))
	if err != nil {
		return fatalf("deployed but fingerprint probe failed: %v", err)
	}
	fmt.Printf("deploy ok: https://%s:%d\n  token: %s\n  fingerprint: %s\n", t.host, port, effToken, fp)

	if name != "" {
		return addFn([]string{name, "--server", fmt.Sprintf("%s:%d", t.host, port), "--token", effToken, "--fingerprint", fp, "--samples", fmt.Sprintf("%d", samples)})
	}
	fmt.Printf("register with: pvm exit add <name> --server %s:%d --token %s --fingerprint %s\n", t.host, port, effToken, fp)
	return 0
}
