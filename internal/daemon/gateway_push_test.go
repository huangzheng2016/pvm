package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func newPushTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("PVM_HOME", t.TempDir())
	p, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	st, err := cluster.LoadStore(p.Home + "/clusters")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{P: p, Clusters: st, vms: map[string]*vm.Spec{}}
	return s
}

func TestRenderGatewayConfigFreshSnapshot(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	stale := s.Clusters.Get(1)

	var first, second []byte
	if err := s.renderGatewayConfigLocked(1, func(cfg []byte) error {
		first = append(first, cfg...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clusters.AddVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	if err := s.renderGatewayConfigLocked(1, func(cfg []byte) error {
		second = append(second, cfg...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(first), "dev1") {
		t.Fatal("first render unexpectedly contains dev1 (test setup broken)")
	}
	if !strings.Contains(string(second), "dev1") {
		t.Fatal("second render must contain dev1 added after the caller's snapshot")
	}
	if strings.Contains(string(first), string(second)) && len(first) == len(second) {
		t.Fatal("renders must differ after a store mutation")
	}
	_ = stale // the stale copy must never be consulted by the render path
}

func TestRenderGatewayConfigSerialized(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	running, maxRunning := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.renderGatewayConfigLocked(1, func([]byte) error {
				mu.Lock()
				running++
				if running > maxRunning {
					maxRunning = running
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				running--
				mu.Unlock()
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxRunning != 1 {
		t.Fatalf("renders must be serialized per cluster, max concurrent = %d", maxRunning)
	}
}

func TestGatewayRemoteTmpAndAtomicInstall(t *testing.T) {
	a := gatewayRemoteTmp()
	time.Sleep(time.Millisecond)
	b := gatewayRemoteTmp()
	if a == b {
		t.Fatal("remote tmp names must be unique per push")
	}
	cmd := gatewayInstallCmd(a)
	if !strings.Contains(cmd, a) {
		t.Fatal("install cmd must reference the unique upload path")
	}
	if !strings.Contains(cmd, "mv -f /etc/pvm/gateway.json.pvm-new /etc/pvm/gateway.json") {
		t.Fatal("install must atomically rename the staged config into place")
	}
	if !strings.Contains(cmd, "rm -f "+a) {
		t.Fatal("install must clean up the uploaded temp file")
	}
}

func TestPushRetryFailsFastWhenGatewayDown(t *testing.T) {
	s := newPushTestServer(t)
	cl, err := s.Clusters.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw"}
	if err := os.MkdirAll(s.P.VMDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = s.pushGatewayConfigRetry(1)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("must fail with gateway-not-running, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("must fail fast, took %v", time.Since(start))
	}
}

func TestRenderGatewayConfigValidJSON(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a", "b", "c"} {
		if _, err := s.Clusters.AddVM(1, name); err != nil {
			t.Fatal(err)
		}
		_ = i
		var out []byte
		if err := s.renderGatewayConfigLocked(1, func(cfg []byte) error {
			out = cfg
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("render %d must be valid json: %v", i, err)
		}
	}
}

func TestRenderGatewayConfigDefaultExit(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetDefaultExit(1, "us"); err != nil {
		t.Fatal(err)
	}
	render := func() []byte {
		var out []byte
		if err := s.renderGatewayConfigLocked(1, func(cfg []byte) error {
			out = cfg
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var doc map[string]any
	if err := json.Unmarshal(render(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["default_exit"] != "us" {
		t.Fatalf("default_exit = %v", doc["default_exit"])
	}
	if err := s.Clusters.SetDefaultExit(1, "direct"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(render()), "default_exit") {
		t.Fatal("cleared default exit must be omitted from the payload")
	}
}
