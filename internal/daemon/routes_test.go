package daemon

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// routeOpsRecorder builds vmRouteOps that record the call order and fail
// on scripted steps.
type routeOpsRecorder struct {
	events   []string
	running  bool
	stopErr  error
	startErr error
	pushErr  error
}

func (r *routeOpsRecorder) ops() vmRouteOps {
	return vmRouteOps{
		running: func() (bool, error) { r.events = append(r.events, "running"); return r.running, nil },
		stop:    func() error { r.events = append(r.events, "stop"); return r.stopErr },
		start:   func() error { r.events = append(r.events, "start"); return r.startErr },
		setExit: func(v string) error { r.events = append(r.events, "setExit:"+v); return nil },
		push:    func() error { r.events = append(r.events, "push"); return r.pushErr },
	}
}

func TestSwitchVMRouteStopsBeforeSetExit(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true}
	_, restarted, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err != nil {
		t.Fatal(err)
	}
	if !restarted {
		t.Fatal("a running vm must be restarted after the switch")
	}
	want := []string{"running", "stop", "setExit:my-exit", "push", "start"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedStaysStopped(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: false}
	_, restarted, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err != nil {
		t.Fatal(err)
	}
	if restarted {
		t.Fatal("a stopped vm must not be started by a route switch")
	}
	want := []string{"running", "setExit:my-exit", "push"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStopFailureKeepsRoute(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true, stopErr: errors.New("qmp stuck")}
	_, _, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err == nil || !strings.Contains(err.Error(), "route unchanged") {
		t.Fatalf("stop failure must leave the route unchanged, got %v", err)
	}
	want := []string{"running", "stop"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("no route step may run after a failed stop, events %v", rec.events)
	}
}

func TestSwitchVMRoutePushFailureRollsBack(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true, pushErr: errors.New("gateway unreachable")}
	_, _, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
	if err == nil {
		t.Fatal("push failure must fail the switch")
	}
	for _, want := range []string{"rolled back to old-exit", "gateway unreachable", "pvm route dev1 --via my-exit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err, want)
		}
	}
	want := []string{"running", "stop", "setExit:my-exit", "push", "setExit:old-exit", "push", "start"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRoutePushFailureRestoreStartFails(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true, pushErr: errors.New("gateway unreachable"), startErr: errors.New("qemu exploded")}
	_, _, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
	if err == nil || !strings.Contains(err.Error(), "pvm start dev1") {
		t.Fatalf("restore start failure must hint `pvm start dev1`, got %v", err)
	}
	if !strings.Contains(err.Error(), "qemu exploded") {
		t.Fatalf("error must keep the restore failure: %v", err)
	}
}

func TestSwitchVMRouteStartFailureKeepsRoute(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true, startErr: errors.New("qemu exploded")}
	_, restarted, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err == nil {
		t.Fatal("start failure must not pretend success")
	}
	for _, want := range []string{"route switched to my-exit", "restarting dev1 failed", "qemu exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err, want)
		}
	}
	if restarted {
		t.Fatal("restarted must be false when the restart failed")
	}
	// The push succeeded, so the route stays switched: no rollback to the
	// previous exit may appear in the call order.
	want := []string{"running", "stop", "setExit:my-exit", "push", "start"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestHandleVMRoutePushFailureRollsBackStore(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clusters.AddVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetVMExit(1, "dev1", "old-exit"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{Name: "new-exit", Server: "vps.example.com:443", Token: "YOUR_TOKEN", Transport: "udp"}); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "pvm", ClusterID: 1}
	if err := os.MkdirAll(s.P.VMDir("dev1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/vms/dev1/route", strings.NewReader(`{"via":"new-exit"}`))
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	// No gateway VM exists in this environment, so the config push fails
	// and the handler must roll the store back to the previous exit.
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500 (gateway push must fail without a gateway vm)", rr.Code)
	}
	if got := s.Clusters.Get(1).VMExits["dev1"]; got != "old-exit" {
		t.Fatalf("route must roll back to old-exit, store has %q", got)
	}
	if !strings.Contains(rr.Body.String(), "rolled back") {
		t.Fatalf("body must say the route rolled back: %s", rr.Body.String())
	}
}

func TestHandleVMRouteRejectsNonClusterVM(t *testing.T) {
	s := newPushTestServer(t)
	spec := &vm.Spec{Name: "gw1", Role: "gateway", User: "pvmgw"}
	if err := os.MkdirAll(s.P.VMDir("gw1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/vms/gw1/route", strings.NewReader(`{"via":"direct"}`))
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("gateway vm must be rejected with 400, got %d", rr.Code)
	}
}

func TestStartForRouteSwitchFailureHintsPvmStart(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	// startVM fails before QEMU launches (no gateway image in this
	// environment); the error must name the exact recovery command.
	err := s.startForRouteSwitch(&vm.Spec{Name: "dev1", Role: "user", User: "pvm", ClusterID: 1})
	if err == nil || !strings.Contains(err.Error(), "pvm start dev1") {
		t.Fatalf("start failure must hint `pvm start dev1`, got %v", err)
	}
}
