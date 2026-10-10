package daemon

import (
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

func TestDaemonRunLockSingleInstance(t *testing.T) {
	p, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	p.Run = t.TempDir()
	hold, err := fsutil.TryLock(p.DaemonRunLock())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Unlock()

	err = Run(p, &Server{})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second daemon must be refused, got %v", err)
	}
}

func TestRelayEnsurePoolPortMismatch(t *testing.T) {
	r := newRelaySet()
	defer r.Remove("tcp", 0)
	if err := r.Ensure("tcp", 0, 30000); err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure("tcp", 0, 30000); err != nil {
		t.Fatalf("same pool port must be a no-op: %v", err)
	}
	err := r.Ensure("tcp", 0, 30001)
	if err == nil || !strings.Contains(err.Error(), "already relayed to pool port 30000") {
		t.Fatalf("pool port mismatch must be an explicit error, got %v", err)
	}
}

func TestKasmProxyTargetPortRebuild(t *testing.T) {
	s := &Server{}
	px1, err := s.kasmProxyFor("dev1", 31000, "pw1")
	if err != nil {
		t.Fatal(err)
	}
	px2, err := s.kasmProxyFor("dev1", 31000, "pw2")
	if err != nil {
		t.Fatal(err)
	}
	if px1 != px2 {
		t.Fatal("same target port should reuse the proxy")
	}
	if got := px2.pw.Load().(string); got != "pw2" {
		t.Fatalf("credential should rotate in place, got %q", got)
	}
	px3, err := s.kasmProxyFor("dev1", 31001, "pw3")
	if err != nil {
		t.Fatal(err)
	}
	if px3 == px2 {
		t.Fatal("changed target port must rebuild the proxy")
	}
	if px3.targetPort != 31001 {
		t.Fatalf("new proxy bound to wrong target: %d", px3.targetPort)
	}
	for _, px := range []*kasmProxy{px1, px3} {
		px.close()
	}
}
