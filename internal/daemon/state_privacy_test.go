package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestSpecCacheReturnsIndependentSnapshots(t *testing.T) {
	s := &Server{}
	spec := &vm.Spec{Name: "dev1", Mounts: []vm.GuestMount{{Tag: "pvm0", Guest: "/mnt/data"}}}
	s.cacheSpec(spec)
	spec.Mounts[0].Guest = "/mnt/changed"
	first, err := s.getSpec("dev1")
	if err != nil {
		t.Fatal(err)
	}
	first.Mounts[0].Guest = "/mnt/reader"
	second, err := s.getSpec("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Mounts[0].Guest != "/mnt/data" {
		t.Fatal("cache shares mutable mount slice")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				next, err := s.getSpec("dev1")
				if err != nil {
					t.Error(err)
					return
				}
				next.MemMB++
				s.cacheSpec(next)
			}
		}()
	}
	wg.Wait()
}

func TestProvisionGenerationRejectsStaleCompletion(t *testing.T) {
	s := &Server{}
	old := s.progressGeneration("dev1")
	s.resetStages("dev1")
	s.stageIfCurrent("dev1", old, "ready", "stale completion")
	if s.vmReady("dev1") {
		t.Fatal("old operation marked new VM ready")
	}
	seq := s.progressGeneration("dev1")
	s.stageIfCurrent("dev1", seq, "ssh", "SSH reachable")
	if s.vmReady("dev1") {
		t.Fatal("SSH alone is not readiness")
	}
	s.stageIfCurrent("dev1", seq, "ready", "agent reachable")
	if !s.vmReady("dev1") {
		t.Fatal("agent completion not ready")
	}
	s.stageIfCurrent("dev1", seq, "failed", "lost installation")
	if s.vmReady("dev1") {
		t.Fatal("failure did not invalidate readiness")
	}
}

func TestMissingAgentAssetReportsFailure(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	s.ensureAgentAsync(spec)
	events := s.stages(spec.Name)
	if len(events) != 1 || events[0].Stage != "failed" || !strings.Contains(events[0].Detail, "pvm stop") {
		t.Fatalf("stages = %+v", events)
	}
}

func TestAgentDialGateHonorsCancellation(t *testing.T) {
	s := &Server{}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	s.agentDialLocks.Store("dev1", gate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := s.agentClientContext(ctx, "dev1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled dial waited for gate")
	}
}

func TestReadOnlyMountStoppedLifecycle(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	body, _ := json.Marshal(map[string]any{"host": host, "guest": "/mnt/data", "read_only": true})
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/vms/dev1/mounts", bytes.NewReader(body)))
	if rr.Code != 201 {
		t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
	}
	loaded, err := vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Mounts) != 1 || !loaded.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v", loaded.Mounts)
	}
	cfg := loaded.QEMUConfig(s.P, s.Accel, "", 0, 0)
	if len(cfg.ExtraMounts) != 1 || !cfg.ExtraMounts[0].ReadOnly {
		t.Fatal("read-only not passed to QEMU backend")
	}
	script, err := guestMountReconcileScript(loaded.Mounts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, ",ro") {
		t.Fatalf("missing read-only fstab: %s", script)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("DELETE", "/v1/vms/dev1/mounts?guest="+url.QueryEscape("/mnt/data"), nil))
	if rr.Code != 200 {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
	}
	loaded, err = vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Mounts) != 0 {
		t.Fatal("removed mount remains in state")
	}
}

func TestGatewayInstallRestrictsCredentialPermissions(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "upload.json")
	target := filepath.Join(dir, "etc-pvm")
	if err := os.WriteFile(remote, []byte(`{"token":"YOUR_TOKEN"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := gatewayInstallCmd(remote)
	if !strings.Contains(cmd, "-o root -g root") {
		t.Fatal("install must assign root ownership")
	}
	// Run the real install/rename commands inside an unprivileged fixture.
	cmd = strings.ReplaceAll(cmd, "sudo ", "")
	cmd = strings.ReplaceAll(cmd, "-o root -g root ", "")
	cmd = strings.ReplaceAll(cmd, "/etc/pvm", target)
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("install: %v: %s", err, out)
	}
	for path, want := range map[string]os.FileMode{target: 0700, filepath.Join(target, "gateway.json"): 0600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Fatalf("mode %o, want %o", fi.Mode().Perm(), want)
		}
	}
	if _, err := os.Stat(remote); !os.IsNotExist(err) {
		t.Fatalf("upload not removed: %v", err)
	}
}

func TestDesktopPasswordNeverEntersRequestQuery(t *testing.T) {
	raw := desktopURL(&desktopState{Password: "YOUR_TOKEN", HostPort: 18080}, "dev1")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" || strings.Contains(u.RequestURI(), "YOUR_TOKEN") {
		t.Fatal("password sent in HTTP request")
	}
	values, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("password") != "YOUR_TOKEN" || values.Get("vm") != "dev1" {
		t.Fatal("noVNC fragment settings missing")
	}
}
