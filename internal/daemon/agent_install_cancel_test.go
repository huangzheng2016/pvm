package daemon

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestAgentInstallationCancellationStopsOldSideEffects(t *testing.T) {
	for _, phase := range []string{"ssh", "scp"} {
		for _, action := range []string{"reset", "stop", "delete"} {
			t.Run(phase+"/"+action, func(t *testing.T) {
				s, spec, _ := newMountTestServer(t)
				spec.CreatedAt = time.Now()
				if err := spec.Save(s.P); err != nil {
					t.Fatal(err)
				}
				s.cacheSpec(spec)
				dir := t.TempDir()
				blocked := filepath.Join(dir, "blocked")
				installed := filepath.Join(dir, "installed")
				t.Setenv("PVM_TEST_INSTALL_BLOCKED", blocked)
				t.Setenv("PVM_TEST_INSTALL_MUTATED", installed)
				t.Setenv("PVM_TEST_INSTALL_PHASE", phase)
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				script := `#!/bin/sh
set -eu
name=${0##*/}
if [ "$name" = "$PVM_TEST_INSTALL_PHASE" ]; then
 printf blocked > "$PVM_TEST_INSTALL_BLOCKED"
 exec /bin/sleep 60
fi
for arg do
 case "$arg" in *systemctl*) printf installed > "$PVM_TEST_INSTALL_MUTATED";; esac
done
`
				for _, name := range []string{"ssh", "scp"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				task := s.beginAgentInstall(spec.Name)
				done := make(chan error, 1)
				go func() {
					err := s.installAgentGeneration(task.ctx, spec, "fixture-agent", task.seq)
					s.finishAgentInstall(spec.Name, task)
					done <- err
				}()
				t.Cleanup(func() { task.cancel() })
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(blocked); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("fake command did not block")
					}
					time.Sleep(time.Millisecond)
				}
				if phase == "ssh" {
					lock := qemu.LockVM(spec.Name)
					if !lock.TryLock() {
						t.Fatal("SSH readiness held lifecycle lock")
					}
					lock.Unlock()
				}
				actionDone := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					rr := httptest.NewRecorder()
					switch action {
					case "reset":
						s.resetStages(spec.Name)
						rr.WriteHeader(200)
					case "stop":
						s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/vms/dev1/stop", nil))
					case "delete":
						s.handler().ServeHTTP(rr, httptest.NewRequest("DELETE", "/v1/vms/dev1", nil))
					}
					actionDone <- rr
				}()
				select {
				case rr := <-actionDone:
					if rr.Code != 200 {
						t.Fatalf("%s: %d %s", action, rr.Code, rr.Body.String())
					}
				case <-time.After(10 * time.Second):
					t.Fatal("lifecycle change could not cancel blocked installation")
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("cancelled installation succeeded")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled child did not terminate")
				}
				// A new instance can reuse the name after lifecycle cancellation.
				fresh := *spec
				fresh.CreatedAt = spec.CreatedAt.Add(time.Second)
				if err := os.MkdirAll(s.P.VMDir(spec.Name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := fresh.Save(s.P); err != nil {
					t.Fatal(err)
				}
				s.cacheSpec(&fresh)
				if _, err := os.Stat(installed); !os.IsNotExist(err) {
					t.Fatal("old installation reached systemctl after cancellation")
				}
				if s.vmReady(spec.Name) {
					t.Fatal("cancelled task marked replacement ready")
				}
			})
		}
	}
}

func TestAgentInstallRejectsReplacementBeforeMutation(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	spec.CreatedAt = time.Now()
	fresh := *spec
	fresh.CreatedAt = spec.CreatedAt.Add(time.Second)
	if err := fresh.Save(s.P); err != nil {
		t.Fatal(err)
	}
	task := s.beginAgentInstall(spec.Name)
	defer s.finishAgentInstall(spec.Name, task)
	invoked := false
	err := s.withInstallVM(context.Background(), spec, task.seq, func(*vm.Spec) error { invoked = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "instance changed") || invoked {
		t.Fatalf("stale mutation accepted: %v", err)
	}
}

func TestStartRunningVMResumesCancelledProvisioning(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			s, spec, _ := newMountTestServer(t)
			markVMRunning(t, s, spec.Name)
			if ready {
				s.stage(spec.Name, "ready", "agent reachable")
			}
			old := s.beginAgentInstall(spec.Name)
			if err := os.WriteFile(s.P.AgentBin(), []byte("fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			defer s.resetStages(spec.Name)
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/vms/dev1/start", nil))
			if rr.Code != 200 {
				t.Fatalf("running start failed: %d %s", rr.Code, rr.Body.String())
			}
			if old.ctx.Err() == nil {
				t.Fatal("old task was not cancelled")
			}
			s.progMu.Lock()
			current := s.installTasks[spec.Name]
			s.progMu.Unlock()
			if ready {
				if !s.vmReady(spec.Name) {
					t.Fatal("healthy running VM lost readiness")
				}
				return
			}
			if current == nil || current == old || current.ctx.Err() != nil {
				t.Fatal("running VM was left without a live provisioning task")
			}
			s.resetStages(spec.Name)
			deadline := time.Now().Add(time.Second)
			for {
				s.progMu.Lock()
				pending := s.installTasks[spec.Name]
				s.progMu.Unlock()
				if pending == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("replacement task did not stop")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestResumeAgentInstallAfterFailedRoute(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	old := s.beginAgentInstall(spec.Name)
	s.cancelAgentInstall(spec.Name)
	s.resumeAgentInstallIfRunning(spec.Name)
	if s.progressGeneration(spec.Name) != old.seq {
		t.Fatal("stopped VM began provisioning")
	}
	markVMRunning(t, s, spec.Name)
	if err := os.WriteFile(s.P.AgentBin(), []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer s.resetStages(spec.Name)
	s.resumeAgentInstallIfRunning(spec.Name)
	s.progMu.Lock()
	current := s.installTasks[spec.Name]
	s.progMu.Unlock()
	if current == nil || current == old || current.ctx.Err() != nil {
		t.Fatal("failed route left running VM without readiness task")
	}
	s.resumeAgentInstallIfRunning(spec.Name)
	s.progMu.Lock()
	repeated := s.installTasks[spec.Name]
	s.progMu.Unlock()
	if repeated != current {
		t.Fatal("live readiness task was restarted unnecessarily")
	}
}

func TestStartResetDoesNotCancelNewGatewayTask(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	store, err := cluster.LoadStore(filepath.Join(s.P.Home, "clusters"))
	if err != nil {
		t.Fatal(err)
	}
	s.Clusters = store
	cl, err := store.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVM(1, spec.Name); err != nil {
		t.Fatal(err)
	}
	spec.ClusterID = 1
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	s.cacheSpec(spec)
	gw := &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw", ClusterID: 1, SSHPort: 10022}
	if err := os.MkdirAll(s.P.VMDir(gw.Name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := gw.Save(s.P); err != nil {
		t.Fatal(err)
	}
	s.cacheSpec(gw)
	markVMRunning(t, s, gw.Name)
	oldAttempts, oldDelay := gatewayRetryAttempts, gatewayRetryDelay
	gatewayRetryAttempts, gatewayRetryDelay = 1, time.Millisecond
	t.Cleanup(func() { gatewayRetryAttempts, gatewayRetryDelay = oldAttempts, oldDelay })
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/vms/dev1/start", nil))
	if strings.Contains(rr.Body.String(), "context canceled") {
		t.Fatalf("start reset cancelled its new gateway task: %s", rr.Body.String())
	}
}
