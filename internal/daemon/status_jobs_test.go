package daemon

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestVMStatusPersistsAcrossServerRestart(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	s.setDesiredRunning(spec.Name, true)
	s.stage(spec.Name, "provision", "waiting for cloud-init")
	s.stage(spec.Name, "failed", "cloud-init status \"error\": boom")

	raw, err := os.ReadFile(s.statusPath(spec.Name))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk VMStatus
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if !onDisk.DesiredRunning || onDisk.ProvisionState != ProvisionFailed || onDisk.AgentReady || !strings.Contains(onDisk.LastError, "boom") {
		t.Fatalf("unexpected persisted status: %+v", onDisk)
	}

	// A fresh daemon with empty memory must recover the failure and desired state.
	s2 := NewServer(s.P, s.Cfg, nil, s.Accel, s.Clusters)
	st := s2.getVMStatus(spec.Name)
	if !st.DesiredRunning || st.ProvisionState != ProvisionFailed || !strings.Contains(st.LastError, "boom") {
		t.Fatalf("restarted daemon lost status: %+v", st)
	}
	if s2.vmReady(spec.Name) {
		t.Fatal("failed VM must not report ready after restart")
	}
	if stages := s2.stages(spec.Name); len(stages) == 0 {
		t.Fatal("persisted stages were not restored")
	}
}

func TestJobLifecycleAndWait(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	j := s.startJob("provision", spec.Name)
	s.updateJob(spec.Name, "provision", "running")
	if _, err := s.waitJob(j.ID, 20*time.Millisecond); err == nil {
		t.Fatal("expected wait_timeout")
	} else {
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != "wait_timeout" || !ae.Retryable {
			t.Fatalf("expected retryable wait_timeout APIError, got %v", err)
		}
	}
	s.finishJob(j.ID, nil)
	got, err := s.waitJob(j.ID, time.Second)
	if err != nil || got.Status != JobCompleted {
		t.Fatalf("completed job = %+v, %v", got, err)
	}

	j2 := s.startJob("provision", spec.Name)
	s.finishJob(j2.ID, errors.New("dial failed"))
	req := httptest.NewRequest("GET", "/v1/jobs/"+j2.ID, nil)
	req.SetPathValue("id", j2.ID)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	var failed Job
	if err := json.Unmarshal(rr.Body.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status != JobFailed || failed.Error == nil || failed.Error.Message != "dial failed" {
		t.Fatalf("failed job = %+v", failed)
	}
}

func TestAPIErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code int
		err  error
		want string
	}{
		{404, errors.New("vm \"dev1\" not found"), "not_found"},
		{500, syscall.ENOSPC, "disk_full"},
		{500, errors.New("vm dev1 is not running"), "vm_not_running"},
		{409, errors.New("vm \"dev1\" already exists"), "already_exists"},
		{400, errors.New("bad flag"), "invalid_input"},
	} {
		ae := classifyError(tc.code, tc.err)
		if ae.Code != tc.want || ae.Message == "" {
			t.Errorf("classifyError(%d, %v) = %+v, want code %s", tc.code, tc.err, ae, tc.want)
		}
	}
}
