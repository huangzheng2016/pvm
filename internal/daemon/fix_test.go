package daemon

import (
	"encoding/json"
	"net/http/httptest"
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

func TestFailedRecoveryIsNotReady(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	s.ready.Store(false)
	s.recoverClusters()
	if s.ready.Load() {
		t.Fatal("failed recovery must not be marked ready")
	}
}

func TestHealthReportsRecoveryReadiness(t *testing.T) {
	s := &Server{}
	get := func() map[string]any {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("health status = %d", rr.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := get()["ready"]; got != false {
		t.Fatalf("initial ready = %v, want false", got)
	}
	s.ready.Store(true)
	if got := get()["ready"]; got != true {
		t.Fatalf("recovered ready = %v, want true", got)
	}
}
