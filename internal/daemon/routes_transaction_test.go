package daemon

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestRouteRollbackDoesNotOverwriteConcurrentAdd(t *testing.T) {
	s := newPushTestServer(t)
	cl, err := s.Clusters.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	s.vms[cl.Gateway] = &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw", SSHPort: 2222}
	bin := t.TempDir()
	t.Setenv("PVM_ROUTE_TEST", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	scp := `#!/bin/sh
if mkdir "$PVM_ROUTE_TEST/started" 2>/dev/null; then
  while [ ! -f "$PVM_ROUTE_TEST/release" ]; do sleep 0.01; done
  exit 1
fi
exit 0
`
	for name, script := range map[string]string{"scp": scp, "ssh": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	release := func() { _ = os.WriteFile(filepath.Join(bin, "release"), nil, 0600) }
	defer release()
	run := func(pattern string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/routes", strings.NewReader(`{"type":"domain-suffix","pattern":"`+pattern+`","action":"direct"}`))
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, req)
		return rr
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- run("first.example.com") }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(bin, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first push did not start")
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- run("second.example.com") }()
	// The pending push keeps the first transaction open. A concurrent
	// handler must not mutate the rule set before the rollback completes.
	time.Sleep(30 * time.Millisecond)
	if got := len(s.Clusters.Get(1).Rules); got != 1 {
		t.Errorf("concurrent request mutated pending transaction: %d rules", got)
	}
	release()
	for i, ch := range []chan *httptest.ResponseRecorder{first, second} {
		select {
		case rr := <-ch:
			want := 500
			if i == 1 {
				want = 201
			}
			if rr.Code != want {
				t.Fatalf("request %d status %d: %s", i, rr.Code, rr.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("route transaction hung")
		}
	}
	rs := s.Clusters.Get(1).Rules
	if len(rs) != 1 || rs[0].Pattern != "second.example.com" {
		t.Fatalf("successful concurrent rule lost: %#v", rs)
	}
}

func TestSwitchVMRouteRollbackFailureLeavesVMStopped(t *testing.T) {
	for _, saveFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "push", true: "save"}[saveFails], func(t *testing.T) {
			s := newPushTestServer(t)
			original := errors.New("initial push failed")
			rollback := errors.New("rollback failed")
			pushes, saves, starts := 0, 0, 0
			ops := vmRouteOps{
				running: func() (bool, error) { return true, nil },
				stop:    func() error { return nil },
				start:   func() error { starts++; return nil },
				setExit: func(string) error {
					saves++
					if saves == 2 && saveFails {
						return rollback
					}
					return nil
				},
				push: func() error {
					pushes++
					if pushes == 1 {
						return original
					}
					return rollback
				},
			}
			_, _, err := s.switchVMRoute(&vm.Spec{Name: "dev1"}, "my-exit", "", ops)
			if !errors.Is(err, original) || !errors.Is(err, rollback) {
				t.Fatalf("failure causes lost: %v", err)
			}
			if starts != 0 {
				t.Fatal("VM restarted after rollback failure")
			}
			if saveFails && pushes != 1 {
				t.Fatal("pushed state after rollback save failed")
			}
			if !strings.Contains(err.Error(), "remains stopped") {
				t.Fatalf("missing recovery state: %v", err)
			}
		})
	}
}

func TestRollbackRoutePreservesFailureCauses(t *testing.T) {
	cause := errors.New("push failed")
	rollback := errors.New("save failed")
	pushed := false
	err := rollbackRoute("rule", cause, func() error { return rollback }, func() error { pushed = true; return nil })
	if !errors.Is(err, cause) || !errors.Is(err, rollback) || pushed {
		t.Fatalf("unsafe rollback: err=%v pushed=%v", err, pushed)
	}
	if strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("failed rollback claimed success: %v", err)
	}
}
