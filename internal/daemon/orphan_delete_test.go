package daemon

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deleteVM(s *Server, name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("DELETE", "/v1/vms/"+name, nil)
	req.SetPathValue("name", name)
	rr := httptest.NewRecorder()
	s.handleDelete(rr, req)
	return rr
}

// orphanDev1 removes dev1's files behind the daemon's back, the way
// deleting ~/.pvm/vms by hand does, while its membership stays.
func orphanDev1(t *testing.T, s *Server) {
	t.Helper()
	if err := os.RemoveAll(s.P.VMDir("dev1")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	delete(s.vms, "dev1")
	s.mu.Unlock()
}

func TestDeleteOrphanMemberDropsClusterState(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	m, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port, 80, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.relays.Ensure("tcp", port, m.PoolPort); err != nil {
		t.Fatal(err)
	}
	orphanDev1(t, s)

	rr := deleteVM(s, "dev1")
	if rr.Code == 404 || rr.Code == 409 {
		t.Fatalf("orphan delete = %d %s", rr.Code, rr.Body)
	}
	cl := s.Clusters.Get(dev1.ClusterID)
	if cl.VMs["dev1"] != nil {
		t.Fatal("orphan membership survives delete")
	}
	if cl.VMs["dev2"] == nil {
		t.Fatal("delete removed another member")
	}
	for _, m := range cl.Maps {
		if m.VM == "dev1" {
			t.Fatalf("orphan map survives: %+v", m)
		}
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("orphan relay survives delete")
	}
	if rr := deleteVM(s, "dev1"); rr.Code != 404 {
		t.Fatalf("second delete = %d, want 404", rr.Code)
	}
}

// Without vm.json pvm cannot stop the VM, so a run state that may belong
// to a live QEMU must keep the membership and its address reserved.
func TestDeleteOrphanMemberRefusesUnknownRunState(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	orphanDev1(t, s)
	if err := os.MkdirAll(s.P.VMDir("dev1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.P.VMDir("dev1"), "run.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	rr := deleteVM(s, "dev1")
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "may still be running") {
		t.Fatalf("delete = %d %s, want 409 refusal", rr.Code, rr.Body)
	}
	if s.Clusters.Get(dev1.ClusterID).VMs["dev1"] == nil {
		t.Fatal("refused delete dropped the membership")
	}
}

func TestDeleteUnknownVMNotFound(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	if rr := deleteVM(s, "nope"); rr.Code != 404 || !strings.Contains(rr.Body.String(), `not found`) {
		t.Fatalf("delete = %d %s, want 404 not found", rr.Code, rr.Body)
	}
}
