package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
)

func TestGoarchFor(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64",
	} {
		got, err := goarchFor(in)
		if err != nil || got != want {
			t.Fatalf("goarchFor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"i386", "armv7l", "riscv64", ""} {
		if _, err := goarchFor(bad); err == nil {
			t.Fatalf("goarchFor(%q) should fail", bad)
		}
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	unit := renderSystemdUnit(":18443", "abc123")
	for _, want := range []string{
		"--listen :18443 --token abc123",
		"Restart=always",
		"WantedBy=multi-user.target",
		"pvm exit-agent",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestParseSSHTarget(t *testing.T) {
	cases := map[string]sshTarget{
		"root@1.2.3.4":      {user: "root", host: "1.2.3.4"},
		"root@1.2.3.4:2222": {user: "root", host: "1.2.3.4", port: "2222"},
		"1.2.3.4":           {user: "root", host: "1.2.3.4"},
		"admin@vps.example": {user: "admin", host: "vps.example"},
	}
	for in, want := range cases {
		got, err := parseSSHTarget(in)
		if err != nil || got != want {
			t.Fatalf("parseSSHTarget(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if _, err := parseSSHTarget("@"); err == nil {
		t.Fatal("empty target should fail")
	}
}

// mockDeployScripts records ssh scripts and drives canned outcomes.
type mockDeploy struct {
	scripts    []string
	unameOut   string
	unitExists bool
	healthOK   bool
}

func (m *mockDeploy) hooks() deployHooks {
	return deployHooks{
		ssh: func(_ sshTarget, script string) (string, error) {
			m.scripts = append(m.scripts, script)
			if strings.Contains(script, "uname -s") {
				return m.unameOut, nil
			}
			// deploy script
			var b strings.Builder
			if !m.healthOK {
				b.WriteString("HEALTH_FAIL\nROLLED_BACK\n")
				return b.String(), fmt.Errorf("exit status 1")
			}
			b.WriteString("MODE=systemd\n")
			if m.unitExists {
				b.WriteString("PVM_TOKEN=existingtoken00000000000000000000000000\n")
			} else {
				// freshly generated token echoed back
				if i := strings.Index(script, "TOKEN="); i >= 0 {
					line := script[i:]
					if j := strings.Index(line, "\n"); j > 0 {
						b.WriteString("PVM_" + line[:j] + "\n")
					}
				}
			}
			return b.String(), nil
		},
		scp:   func(_ sshTarget, _, _ string) error { return nil },
		build: func(_, _, _ string) error { return nil },
		probe: func(string) (string, error) { return "deadbeef", nil },
	}
}

func TestDeploySuccessNewUnit(t *testing.T) {
	m := &mockDeploy{unameOut: "Linux\nx86_64\n", healthOK: true}
	var addArgs []string
	code := runExitDeploy(m.hooks(), "root@1.2.3.4", ":18443", "", t.TempDir(), 3, func(args []string) int {
		addArgs = args
		return 0
	})
	if code != 0 {
		t.Fatalf("deploy rc=%d", code)
	}
	joined := strings.Join(m.scripts, "\n")
	if !strings.Contains(joined, "pvm-exit-agent.service") {
		t.Fatal("unit path not referenced")
	}
	if addArgs != nil {
		t.Fatal("add should not be called without --name")
	}
}

func TestDeployRollbackOnHealthFail(t *testing.T) {
	m := &mockDeploy{unameOut: "Linux\nx86_64\n", healthOK: false}
	code := runExitDeploy(m.hooks(), "root@1.2.3.4", ":18443", "", t.TempDir(), 3, nil)
	if code == 0 {
		t.Fatal("deploy should fail when health check fails")
	}
	deployScript := ""
	for _, s := range m.scripts {
		if strings.Contains(s, "mv \"$NEWBIN\" \"$BIN\"") {
			deployScript = s
		}
	}
	if !strings.Contains(deployScript, "ROLLED_BACK") {
		t.Fatal("rollback markers missing from deploy script")
	}
	if !strings.Contains(deployScript, "mv \"$BIN.prev\" \"$BIN\"") {
		t.Fatal("rollback should restore pvm.prev")
	}
}

func TestDeployRejectsNonLinuxAndBadArch(t *testing.T) {
	m := &mockDeploy{unameOut: "FreeBSD\namd64\n"}
	if code := runExitDeploy(m.hooks(), "root@1.2.3.4", ":18443", "", t.TempDir(), 3, nil); code == 0 {
		t.Fatal("non-Linux should fail")
	}
	m = &mockDeploy{unameOut: "Linux\nriscv64\n"}
	if code := runExitDeploy(m.hooks(), "root@1.2.3.4", ":18443", "", t.TempDir(), 3, nil); code == 0 {
		t.Fatal("unsupported arch should fail")
	}
}

func TestDeployRegistersWithName(t *testing.T) {
	m := &mockDeploy{unameOut: "Linux\nx86_64\n", healthOK: true, unitExists: true}
	var got []string
	code := runExitDeploy(m.hooks(), "root@1.2.3.4", ":18443", "prod", t.TempDir(), 5, func(args []string) int {
		got = args
		return 0
	})
	if code != 0 {
		t.Fatalf("deploy rc=%d", code)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "prod") || !strings.Contains(joined, "--fingerprint deadbeef") {
		t.Fatalf("add args missing fingerprint check: %v", got)
	}
	if !strings.Contains(joined, "--token existingtoken00000000000000000000000000") {
		t.Fatalf("existing unit token should be reused: %v", got)
	}
}

var _ = exit.ProbeFingerprint // keep import for the probe type
