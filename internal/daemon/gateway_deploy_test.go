package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func setupGatewayDeployTest(t *testing.T, content string) (*Server, string, string) {
	t.Helper()
	s := newPushTestServer(t)
	cl, err := s.Clusters.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw", SSHPort: 10022}
	if err := os.MkdirAll(s.P.VMDir(spec.Name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	s.cacheSpec(spec)
	bin := filepath.Join(s.P.Runtime, native.GuestBinary("gateway"))
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	log := filepath.Join(t.TempDir(), "commands")
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$PVM_TEST_GW_LOG"
case "$*" in
  *sha256sum*) printf '%s /usr/local/bin/pvm-gateway\n' "$PVM_TEST_GW_SHA" ;;
  *cloud-init*)
    if [ -n "${PVM_TEST_GW_BLOCKED:-}" ]; then
      printf blocked > "$PVM_TEST_GW_BLOCKED"
      exec /bin/sleep 60
    fi
    if [ "${PVM_TEST_GW_CLOUD_INIT_FAIL:-0}" = 1 ]; then
      printf 'status: degraded\n'
      exit 2
    fi
    ;;
esac
`
	for _, name := range []string{"ssh", "scp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PVM_TEST_GW_LOG", log)
	t.Setenv("PVM_TEST_GW_SHA", fmt.Sprintf("%x", digest))
	return s, log, bin
}

func TestGatewayAgentDeploymentSkipsRestartWhenCurrent(t *testing.T) {
	s, log, _ := setupGatewayDeployTest(t, "current gateway")
	cl := s.Clusters.Get(1)
	for i := 0; i < 2; i++ {
		if err := s.deployGatewayAgentCtx(context.Background(), cl); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "cloud-init") || strings.Contains(string(data), "systemctl restart") {
		t.Fatalf("healthy gateway was deployed again:\n%s", data)
	}
	if strings.Count(string(data), "sha256sum") != 2 {
		t.Fatalf("each start should inspect the running agent:\n%s", data)
	}
}

func TestGatewayAgentDeploymentRestartsForNewBinary(t *testing.T) {
	s, log, bin := setupGatewayDeployTest(t, "old gateway")
	cl := s.Clusters.Get(1)
	if err := os.WriteFile(bin, []byte("new gateway"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.deployGatewayAgentCtx(context.Background(), cl); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cloud-init", "install -m 0755", "systemctl restart"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in gateway deployment:\n%s", want, data)
		}
	}
}

func TestGatewayAgentDeploymentFailsWhenCloudInitFails(t *testing.T) {
	s, log, bin := setupGatewayDeployTest(t, "old gateway")
	if err := os.WriteFile(bin, []byte("new gateway"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PVM_TEST_GW_CLOUD_INIT_FAIL", "1")
	err := s.deployGatewayAgentCtx(context.Background(), s.Clusters.Get(1))
	if err == nil || !strings.Contains(err.Error(), "cloud-init wait") || !strings.Contains(err.Error(), "status: degraded") {
		t.Fatalf("cloud-init failure must stop deployment with diagnostics: %v", err)
	}
	data, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), "install -m 0755") || strings.Contains(string(data), "systemctl restart") {
		t.Fatalf("deployment continued after cloud-init failure:\n%s", data)
	}
}

func TestGatewayDeploymentDoesNotBlockLifecycle(t *testing.T) {
	for _, action := range []string{"stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			s, _, _ := setupGatewayDeployTest(t, "new gateway")
			proc := exec.Command("/bin/sleep", "30")
			if err := proc.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() }()
			ident, err := qemu.IdentityOf(proc.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			state, err := json.Marshal(qemu.RunState{PID: proc.Process.Pid, Ident: ident, Started: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.P.VMDir(s.Clusters.Get(1).Gateway), "run.json"), state, 0600); err != nil {
				t.Fatal(err)
			}
			blocked := filepath.Join(t.TempDir(), "blocked")
			t.Setenv("PVM_TEST_GW_BLOCKED", blocked)
			t.Setenv("PVM_TEST_GW_SHA", strings.Repeat("0", sha256.Size*2))
			task := s.beginAgentInstall(s.Clusters.Get(1).Gateway)
			done := make(chan error, 1)
			go func() {
				done <- s.deployGatewayAgentCtx(task.ctx, s.Clusters.Get(1))
				s.finishAgentInstall(s.Clusters.Get(1).Gateway, task)
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(blocked); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("gateway deployment did not reach cancellable wait")
				}
				time.Sleep(time.Millisecond)
			}
			uri := "/v1/vms/" + s.Clusters.Get(1).Gateway + "/" + action
			if action == "delete" {
				uri = "/v1/vms/" + s.Clusters.Get(1).Gateway
			}
			req := httptest.NewRequest(map[string]string{"stop": "POST", "delete": "DELETE"}[action], uri, nil)
			rr := httptest.NewRecorder()
			actionDone := make(chan struct{})
			go func() {
				s.handler().ServeHTTP(rr, req)
				close(actionDone)
			}()
			select {
			case <-actionDone:
				if rr.Code != 200 {
					t.Fatalf("lifecycle action: %d %s", rr.Code, rr.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lifecycle action blocked behind gateway deployment")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled gateway deployment succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("gateway deployment did not stop")
			}
		})
	}
}

func TestGatewayDNSUpstream(t *testing.T) {
	t.Setenv("PVM_DNS_UPSTREAM", "")
	if got := gatewayDNSUpstream("windows"); got != "tcp://1.1.1.1:53" {
		t.Fatalf("native Windows DNS fallback = %q", got)
	}
	if got := gatewayDNSUpstream("linux"); got != "10.0.2.3" {
		t.Fatalf("Linux DNS upstream changed = %q", got)
	}
	t.Setenv("PVM_DNS_UPSTREAM", " tcp://192.0.2.53:5353 ")
	if got := gatewayDNSUpstream("linux"); got != "tcp://192.0.2.53:5353" {
		t.Fatalf("DNS override = %q", got)
	}
}
