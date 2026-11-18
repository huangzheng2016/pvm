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
	events    []string
	running   bool
	stopErr   error
	startErr  error
	pushErr   error
	pushCalls int
	// gatewayDown makes a stopped VM's switch skip the push.
	gatewayDown bool
	gatewayErr  error
	deferErr    error
}

func (r *routeOpsRecorder) ops() vmRouteOps {
	return vmRouteOps{
		running: func() (bool, error) { r.events = append(r.events, "running"); return r.running, nil },
		stop:    func() error { r.events = append(r.events, "stop"); return r.stopErr },
		start:   func() error { r.events = append(r.events, "start"); return r.startErr },
		setExit: func(v string) error { r.events = append(r.events, "setExit:"+v); return nil },
		push: func() error {
			r.events = append(r.events, "push")
			r.pushCalls++
			if r.pushCalls == 1 {
				return r.pushErr
			}
			return nil
		},
		gatewayRunning: func() (bool, error) {
			r.events = append(r.events, "gateway")
			return !r.gatewayDown, r.gatewayErr
		},
		deferRegion: func() error { r.events = append(r.events, "deferRegion"); return r.deferErr },
	}
}

func TestSwitchVMRouteStopsBeforeSetExit(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true}
	res, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err != nil {
		t.Fatal(err)
	}
	if !res.restarted || !res.pushed {
		t.Fatalf("a running vm must be pushed and restarted, got %+v", res)
	}
	want := []string{"running", "stop", "setExit:my-exit", "push", "start"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedStaysStopped(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: false}
	res, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err != nil {
		t.Fatal(err)
	}
	if res.restarted {
		t.Fatal("a stopped vm must not be started by a route switch")
	}
	if !res.pushed || !strings.Contains(res.note, "when dev1 next starts") {
		t.Fatalf("want a pushed config and a deferred region note, got %+v", res)
	}
	want := []string{"running", "deferRegion", "setExit:my-exit", "gateway", "push"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedWithoutGatewaySavesOnly(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{gatewayDown: true, pushErr: errors.New("gateway unreachable")}
	res, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err != nil || res.restarted || res.pushed {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	want := []string{"running", "deferRegion", "setExit:my-exit", "gateway"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedToDirectDefersOnlyARestore(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{}
	res, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "direct", "old-exit", rec.ops())
	if err != nil || res.note != "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if want := []string{"running", "setExit:direct", "gateway", "push"}; !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
	rec = &routeOpsRecorder{}
	res, err = s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1, PrevTZ: "Asia/Tokyo"}, "direct", "old-exit", rec.ops())
	if err != nil || res.note == "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if want := []string{"running", "deferRegion", "setExit:direct", "gateway", "push"}; !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedDeferFailureKeepsRoute(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{deferErr: errors.New("disk full")}
	_, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
	if err == nil || !strings.Contains(err.Error(), "vm route unchanged") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("got %v", err)
	}
	if want := []string{"running", "deferRegion"}; !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedGatewayCheckFailureRollsBack(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{gatewayErr: errors.New("pid file unreadable")}
	_, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
	if err == nil || !strings.Contains(err.Error(), "vm route unchanged") || !strings.Contains(err.Error(), "pid file unreadable") {
		t.Fatalf("got %v", err)
	}
	want := []string{"running", "deferRegion", "setExit:my-exit", "gateway", "setExit:old-exit"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStoppedPushFailureRollsBack(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{pushErr: errors.New("gateway unreachable")}
	_, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
	if err == nil {
		t.Fatal("push failure must fail the switch")
	}
	for _, want := range []string{"rolled back to old-exit", "gateway unreachable", "pvm route dev1 --via my-exit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err, want)
		}
	}
	// the rollback config is pushed too, and a stopped VM stays stopped
	want := []string{"running", "deferRegion", "setExit:my-exit", "gateway", "push", "setExit:old-exit", "push"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestSwitchVMRouteStopFailureKeepsRoute(t *testing.T) {
	s := newPushTestServer(t)
	rec := &routeOpsRecorder{running: true, stopErr: errors.New("qmp stuck")}
	_, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
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
	_, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
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
	_, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "old-exit", rec.ops())
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
	res, err := s.switchVMRoute(&vm.Spec{Name: "dev1", ClusterID: 1}, "my-exit", "", rec.ops())
	if err == nil {
		t.Fatal("start failure must not pretend success")
	}
	for _, want := range []string{"route switched to my-exit", "restarting dev1 failed", "qemu exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err, want)
		}
	}
	if res.restarted {
		t.Fatal("restarted must be false when the restart failed")
	}
	// The push succeeded, so the route stays switched: no rollback to the
	// previous exit may appear in the call order.
	want := []string{"running", "stop", "setExit:my-exit", "push", "start"}
	if !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("call order = %v, want %v", rec.events, want)
	}
}

func TestHandleVMRouteStoppedWithoutGatewaySavesExit(t *testing.T) {
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
	// No gateway VM exists and dev1 is stopped: the exit is saved for the
	// next start, which pushes the config before dev1 boots.
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if got := s.Clusters.Get(1).VMExits["dev1"]; got != "new-exit" {
		t.Fatalf("store has exit %q, want new-exit", got)
	}
	if !strings.Contains(rr.Body.String(), `"pushed":false`) {
		t.Fatalf("the response must say nothing was pushed: %s", rr.Body.String())
	}
	saved, err := vm.Load(s.P, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.RegionPending {
		t.Fatal("the exit's timezone/locale must be left to the next start")
	}
}

// A gateway whose state cannot be read fails the switch, and
// the handler puts the stored exit back.
func TestHandleVMRouteGatewayCheckFailureRollsBackStore(t *testing.T) {
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
	for _, spec := range []*vm.Spec{
		{Name: "dev1", Role: "user", User: "pvm", ClusterID: 1},
		{Name: s.Clusters.Get(1).Gateway, Role: "gateway", User: "pvmgw"},
	} {
		if err := os.MkdirAll(s.P.VMDir(spec.Name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := spec.Save(s.P); err != nil {
			t.Fatal(err)
		}
	}
	// an unreadable run state: whether the gateway runs is unknown
	gw, err := vm.Load(s.P, s.Clusters.Get(1).Gateway)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.instance(gw).StatePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/vms/dev1/route", strings.NewReader(`{"via":"new-exit"}`))
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 500 || !strings.Contains(rr.Body.String(), "vm route unchanged") {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := s.Clusters.Get(1).VMExits["dev1"]; got != "old-exit" {
		t.Fatalf("store has exit %q, want old-exit", got)
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

func TestApplyPendingRegion(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clusters.AddVM(1, "dev1"); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), &exit.Entry{Name: "my-exit", Server: "vps.example.com:443", Token: "YOUR_TOKEN", Transport: "udp", TZ: "Asia/Tokyo"}); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "pvm", ClusterID: 1, RegionPending: true}
	if err := os.MkdirAll(s.P.VMDir("dev1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}

	// The guest cannot be reached (no gateway VM here): the start must
	// fail and keep the mark, not report a VM on the exit's region.
	if err := s.Clusters.SetVMExit(1, "dev1", "my-exit"); err != nil {
		t.Fatal(err)
	}
	err := s.applyPendingRegion(spec)
	if err == nil || !strings.Contains(err.Error(), "timezone/locale for route my-exit not set") {
		t.Fatalf("got %v", err)
	}
	if saved, _ := vm.Load(s.P, "dev1"); saved == nil || !saved.RegionPending {
		t.Fatal("a failed apply must keep the mark for the next start")
	}

	// Nothing to set for a VM without its own exit: the mark is cleared.
	if err := s.Clusters.SetVMExit(1, "dev1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.applyPendingRegion(spec); err != nil {
		t.Fatal(err)
	}
	if saved, _ := vm.Load(s.P, "dev1"); saved == nil || saved.RegionPending {
		t.Fatal("the mark must be cleared once the region is settled")
	}
}
