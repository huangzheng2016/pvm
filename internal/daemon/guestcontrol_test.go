package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

func TestGuestMapListScoped(t *testing.T) {
	s, dev1, dev2 := newMapTestServer(t)
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, 18080, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev2", dev2.IP, 18081, 81, "tcp"); err != nil {
		t.Fatal(err)
	}
	payload, err := s.guestMapList("dev1")
	if err != nil {
		t.Fatal(err)
	}
	var resp agent.MapListResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Maps) != 1 || resp.Maps[0].HostPort != 18080 || resp.Maps[0].GuestPort != 80 {
		t.Fatalf("dev1 list must contain only dev1's maps, got %+v", resp.Maps)
	}
}

func TestGuestMapAddAndRemove(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	h := s.guestControlHandlerWithPush("dev1", okPush)
	port := freePort(t, "tcp")

	addReq, _ := json.Marshal(agent.MapAddRequest{HostPort: port, GuestPort: 80, Proto: "tcp"})
	payload, err := h(&agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: addReq})
	if err != nil {
		t.Fatal(err)
	}
	var addResp agent.MapAddResponse
	if err := json.Unmarshal(payload, &addResp); err != nil {
		t.Fatal(err)
	}
	if addResp.Map.HostPort != port || addResp.Map.GuestPort != 80 {
		t.Fatalf("bad add response %+v", addResp)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 || got[0].VM != "dev1" {
		t.Fatalf("store after add: %+v", got)
	}

	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port, Proto: "tcp"})
	if _, err := h(&agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq}); err != nil {
		t.Fatal(err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("store after remove: %+v", got)
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("relay must be gone after remove")
	}
}

func TestGuestMapRemoveCrossVMRejected(t *testing.T) {
	s, dev1, dev2 := newMapTestServer(t)
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev2", dev2.IP, 18091, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	h := s.guestControlHandlerWithPush("dev1", okPush)
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: 18091, Proto: "tcp"})
	_, err := h(&agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})
	if err == nil || !strings.Contains(err.Error(), "not mapped by this vm") {
		t.Fatalf("cross-vm remove = %v, want ownership error", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 || got[0].VM != "dev2" {
		t.Fatalf("dev2's map must survive, store: %+v", got)
	}
	// dev2 itself may remove it.
	h2 := s.guestControlHandlerWithPush("dev2", okPush)
	if _, err := h2(&agent.Frame{ID: "r2", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq}); err != nil {
		t.Fatalf("owner remove: %v", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("store after owner remove: %+v", got)
	}
}

func TestGuestMapRemoveMissing(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: 18092})
	_, err := h(&agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})
	if err == nil || !strings.Contains(err.Error(), "no tcp map on port 18092") {
		t.Fatalf("missing remove = %v", err)
	}
}

func TestGuestMapRemoveRollbackOnPushFailure(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, 18093, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	push := func(clusterID int) error { return errors.New("gateway away") }
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: 18093})
	_, err := s.guestMapRemove("dev1", rmReq, push)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("remove with failing push = %v, want rollback", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 {
		t.Fatalf("map must be restored after failed push, store: %+v", got)
	}
}

func TestGuestControlUnknownAction(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	_, err := h(&agent.Frame{ID: "x1", Op: agent.OpControl, Action: "vm.delete"})
	if err == nil || !strings.Contains(err.Error(), "unknown control action") {
		t.Fatalf("unknown action = %v", err)
	}
}

func TestGuestMapAddMalformed(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	if _, err := h(&agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: json.RawMessage(`{`)}); err == nil ||
		!strings.Contains(err.Error(), "bad map.add payload") {
		t.Fatalf("bad json = %v", err)
	}
	bad, _ := json.Marshal(agent.MapAddRequest{GuestPort: 0})
	if _, err := h(&agent.Frame{ID: "a2", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: bad}); err == nil ||
		!strings.Contains(err.Error(), "guest_port") {
		t.Fatalf("bad guest port = %v", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("no map may be created, store: %+v", got)
	}
}

func TestGuestMapAddBadProto(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	req, _ := json.Marshal(agent.MapAddRequest{HostPort: freePort(t, "tcp"), GuestPort: 80, Proto: "sctp"})
	_, err := h(&agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: req})
	if err == nil || !strings.Contains(err.Error(), "proto must be tcp|udp") {
		t.Fatalf("bad proto = %v", err)
	}
}

// fakeGuestAgent stands in for the QEMU chardev end of the agent
// channel: it answers pings and can issue one control request per
// connection, recording the result frames it receives.
type fakeGuestAgent struct {
	ln net.Listener

	mu       sync.Mutex
	conns    int
	maxConns int
	request  *agent.Frame // sent once after the first pong
	results  chan *agent.Frame
}

func startFakeGuest(t *testing.T, sock string, request *agent.Frame) *fakeGuestAgent {
	t.Helper()
	os.MkdirAll(filepath.Dir(sock), 0o700)
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGuestAgent{ln: ln, request: request, results: make(chan *agent.Frame, 4)}
	go f.accept()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeGuestAgent) accept() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns++
		if f.conns > f.maxConns {
			f.maxConns = f.conns
		}
		f.mu.Unlock()
		go f.serve(conn)
	}
}

func (f *fakeGuestAgent) serve(conn net.Conn) {
	defer conn.Close()
	defer func() {
		f.mu.Lock()
		f.conns--
		f.mu.Unlock()
	}()
	r := bufio.NewReaderSize(conn, 64<<10)
	write := func(fr *agent.Frame) error {
		data, err := json.Marshal(fr)
		if err != nil {
			return err
		}
		_, err = conn.Write(append(data, '\n'))
		return err
	}
	for {
		var fr agent.Frame
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		if err := json.Unmarshal(line, &fr); err != nil {
			return
		}
		switch fr.Op {
		case agent.OpPing:
			if err := write(&agent.Frame{ID: fr.ID, Op: agent.OpPong, Version: "test"}); err != nil {
				return
			}
			f.mu.Lock()
			req := f.request
			f.request = nil
			f.mu.Unlock()
			if req != nil {
				if err := write(req); err != nil {
					return
				}
			}
		case agent.OpResult:
			resp := fr
			f.results <- &resp
		}
	}
}

func (f *fakeGuestAgent) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeGuestAgent) maxConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxConns
}

// markVMRunning fabricates a run.json pointing at the test process so
// the control watcher sees a running VM.
func markVMRunning(t *testing.T, s *Server, name string) {
	t.Helper()
	ident, err := qemu.IdentityOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	st := qemu.RunState{PID: os.Getpid(), Ident: ident, Started: time.Now()}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.P.VMDir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.P.VMDir(name), "run.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dropCachedAgent(s *Server, name string) {
	s.agentMu.Lock()
	c := s.agents[name]
	delete(s.agents, name)
	s.agentMu.Unlock()
	if c != nil {
		c.Close()
	}
}

func cachedAgentCount(s *Server, name string) int {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agents[name] == nil {
		return 0
	}
	return 1
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestControlWatchLoopConnectReconnectStop(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	markVMRunning(t, s, "dev1")

	fake := startFakeGuest(t, s.P.AgentSock("dev1"), nil)
	s.watchVM(dev1) // bumps the watch generation like a real start
	s.ensureControlWatcher(dev1)

	waitFor(t, 5*time.Second, func() bool { return fake.connections() == 1 })
	if got := cachedAgentCount(s, "dev1"); got != 1 {
		t.Fatalf("cached agent clients = %d", got)
	}

	// Dropping the channel makes the watcher redial, not stack.
	dropCachedAgent(s, "dev1")
	waitFor(t, 5*time.Second, func() bool { return fake.connections() == 1 })
	if fake.maxConnections() > 1 {
		t.Fatalf("watcher leaked parallel connections, max %d", fake.maxConnections())
	}

	// A repeated ensure for the same generation must not start a second
	// watcher.
	s.ensureControlWatcher(dev1)
	time.Sleep(200 * time.Millisecond)
	if fake.maxConnections() > 1 {
		t.Fatalf("duplicate watcher, max connections %d", fake.maxConnections())
	}

	// VM stops: the watcher exits instead of reconnecting forever.
	os.Remove(filepath.Join(s.P.VMDir("dev1"), "run.json"))
	dropCachedAgent(s, "dev1")
	waitFor(t, 5*time.Second, func() bool { return fake.connections() == 0 })
	time.Sleep(300 * time.Millisecond)
	if fake.connections() != 0 {
		t.Fatal("watcher reconnected after the VM stopped")
	}
}

func TestControlWatchLoopSupersededByRestart(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	markVMRunning(t, s, "dev1")
	fake := startFakeGuest(t, s.P.AgentSock("dev1"), nil)

	s.watchVM(dev1)
	s.ensureControlWatcher(dev1)
	waitFor(t, 5*time.Second, func() bool { return fake.connections() == 1 })

	// A newer start supersedes the generation; the old watcher exits and
	// the new ensure adopts the fresh generation without stacking.
	s.watchVM(dev1)
	s.ensureControlWatcher(dev1)
	waitFor(t, 5*time.Second, func() bool { return fake.connections() == 1 })
	time.Sleep(200 * time.Millisecond)
	if fake.maxConnections() > 1 {
		t.Fatalf("restart stacked watchers, max connections %d", fake.maxConnections())
	}
}

// The full host path: the guest issues map.list on the virtio channel
// and the daemon's channel-bound handler answers with only that VM's
// maps, dispatched through the real client readLoop.
func TestAgentClientControlEndToEnd(t *testing.T) {
	s, dev1, dev2 := newMapTestServer(t)
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, 18080, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev2", dev2.IP, 18081, 81, "tcp"); err != nil {
		t.Fatal(err)
	}

	req := &agent.Frame{ID: "g1", Op: agent.OpControl, Action: agent.ControlMapList}
	fake := startFakeGuest(t, s.P.AgentSock("dev1"), req)

	ac, err := s.agentClient("dev1") // dials, pings; the fake then sends the control request
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()

	select {
	case res := <-fake.results:
		if res.Error != "" {
			t.Fatalf("control error: %s", res.Error)
		}
		var list agent.MapListResponse
		if err := json.Unmarshal(res.Payload, &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Maps) != 1 || list.Maps[0].HostPort != 18080 {
			t.Fatalf("guest on dev1's channel must see only dev1's maps, got %+v", list.Maps)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no control result on the agent channel")
	}
}
