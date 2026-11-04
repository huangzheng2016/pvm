package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
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
	payload, err := h(context.Background(), &agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: addReq})
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
	if _, err := h(context.Background(), &agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq}); err != nil {
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
	_, err := h(context.Background(), &agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})
	if err == nil || !strings.Contains(err.Error(), "no tcp map on port 18091") {
		t.Fatalf("cross-vm remove = %v, want not-found error", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 || got[0].VM != "dev2" {
		t.Fatalf("dev2's map must survive, store: %+v", got)
	}
	// dev2 itself may remove it.
	h2 := s.guestControlHandlerWithPush("dev2", okPush)
	if _, err := h2(context.Background(), &agent.Frame{ID: "r2", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq}); err != nil {
		t.Fatalf("owner remove: %v", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("store after owner remove: %+v", got)
	}
}

// A guest must not be able to distinguish "port mapped by another VM"
// from "port not mapped at all": for the same port, the error must be
// identical whether another VM owns it or nothing is mapped.
func TestGuestMapRemoveErrorIsIndistinguishable(t *testing.T) {
	s, dev1, dev2 := newMapTestServer(t)
	const port = 18091
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev2", dev2.IP, port, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	h := s.guestControlHandlerWithPush("dev1", okPush)
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port, Proto: "tcp"})
	_, errOwned := h(context.Background(), &agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})

	// Same port, now unmapped: the error must read exactly the same.
	if _, err := s.Clusters.RemoveMap(dev1.ClusterID, port, "tcp"); err != nil {
		t.Fatal(err)
	}
	_, errUnmapped := h(context.Background(), &agent.Frame{ID: "r2", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})

	if errOwned == nil || errUnmapped == nil {
		t.Fatalf("both removes must fail: %v / %v", errOwned, errUnmapped)
	}
	if errOwned.Error() != errUnmapped.Error() {
		t.Fatalf("errors must be indistinguishable: %q vs %q", errOwned, errUnmapped)
	}
}

func TestGuestMapRemoveMissing(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: 18092})
	_, err := h(context.Background(), &agent.Frame{ID: "r1", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})
	if err == nil || !strings.Contains(err.Error(), "no tcp map on port 18092") {
		t.Fatalf("missing remove = %v", err)
	}
}

func TestGuestMapRemoveRollbackOnPushFailure(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	push := func(ctx context.Context, clusterID int) error {
		pushes++
		return errors.New("gateway away")
	}
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port})
	_, err := s.guestMapRemove(context.Background(), "dev1", rmReq, push)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("remove with failing push = %v, want rollback", err)
	}
	if pushes != 2 {
		t.Fatalf("push calls = %d, want remove push + rollback re-push", pushes)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 {
		t.Fatalf("map must be restored after failed push, store: %+v", got)
	}
}

// The first push may apply the config and still return an error; the
// rollback must re-push the restored state and succeed.
func TestGuestMapRemoveRollbackRepushesRestoredState(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	push := func(ctx context.Context, clusterID int) error {
		pushes++
		if pushes == 1 {
			return errors.New("apply acknowledged then ssh dropped")
		}
		return nil
	}
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port})
	_, err := s.guestMapRemove(context.Background(), "dev1", rmReq, push)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("remove = %v, want rolled-back error", err)
	}
	if pushes != 2 {
		t.Fatalf("push calls = %d, want the restore push to run", pushes)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 || got[0].HostPort != port {
		t.Fatalf("restored store: %+v", got)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("relay must be restored by the rollback")
	}
}

func TestGuestControlUnknownAction(t *testing.T) {
	s, _, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	_, err := h(context.Background(), &agent.Frame{ID: "x1", Op: agent.OpControl, Action: "vm.delete"})
	if err == nil || !strings.Contains(err.Error(), "unknown control action") {
		t.Fatalf("unknown action = %v", err)
	}
}

func TestGuestMapAddMalformed(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	h := s.guestControlHandler("dev1")
	if _, err := h(context.Background(), &agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: json.RawMessage(`{`)}); err == nil ||
		!strings.Contains(err.Error(), "bad map.add payload") {
		t.Fatalf("bad json = %v", err)
	}
	bad, _ := json.Marshal(agent.MapAddRequest{GuestPort: 0})
	if _, err := h(context.Background(), &agent.Frame{ID: "a2", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: bad}); err == nil ||
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
	_, err := h(context.Background(), &agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: req})
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

// A daemon restart leaves both the watch generation and the watcher
// registry at zero; recovery must still start the control watcher for
// VMs that stayed up (no watchVM bump happens in that path).
func TestEnsureControlWatcherSeqZeroRecover(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	markVMRunning(t, s, "dev1")
	fake := startFakeGuest(t, s.P.AgentSock("dev1"), nil)

	s.ensureControlWatcher(dev1) // no watchVM: the daemon-restart path
	waitFor(t, 5*time.Second, func() bool { return fake.connections() == 1 })

	// A second ensure for the same generation must not stack a watcher.
	s.ensureControlWatcher(dev1)
	time.Sleep(200 * time.Millisecond)
	if fake.maxConnections() > 1 {
		t.Fatalf("duplicate watcher, max connections %d", fake.maxConnections())
	}
}

// A port persisted by another cluster without a live relay (recovery
// failed or has not run) is still taken: neither the guest nor the host
// add path may hijack it.
func TestMapAddCrossClusterPersistedPortRejected(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	cl2, err := s.Clusters.Ensure(2)
	if err != nil {
		t.Fatal(err)
	}
	vnet, err := s.Clusters.AddVM(cl2.ID, "dev9")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t, "tcp")
	if _, err := s.Clusters.AddMap(cl2.ID, "dev9", vnet.IP, port, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	// No relay is bound for cluster 2's map (recovery failure case):
	// the port is only persisted in the store.
	if err := portFree("tcp", port); err != nil {
		t.Fatal("test needs the port unbound on the host")
	}

	h := s.guestControlHandlerWithPush("dev1", okPush)
	addReq, _ := json.Marshal(agent.MapAddRequest{HostPort: port, GuestPort: 80, Proto: "tcp"})
	_, err = h(context.Background(), &agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: addReq})
	if err == nil || !strings.Contains(err.Error(), "already mapped") {
		t.Fatalf("guest add on persisted port = %v, want conflict", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("no map may be created in cluster 1, store: %+v", got)
	}

	// The host add path shares the same global check.
	m, code, err := s.addMap(context.Background(), dev1, port, 80, "tcp", s.relays.Ensure, okPush)
	if err == nil || code != 409 {
		t.Fatalf("host add on persisted port = %v %d %v, want 409", m, code, err)
	}
}

// Concurrent adds of the same port serialize: exactly one wins, and
// concurrent add/remove mixes leave the store consistent.
func TestMapMutationSerializedConcurrent(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	h := s.guestControlHandlerWithPush("dev1", okPush)

	const n = 8
	var wg sync.WaitGroup
	addReq, _ := json.Marshal(agent.MapAddRequest{HostPort: port, GuestPort: 80, Proto: "tcp"})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h(context.Background(), &agent.Frame{ID: "a", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: addReq})
		}()
	}
	wg.Wait()
	maps := s.Clusters.Get(dev1.ClusterID).Maps
	if len(maps) != 1 || maps[0].HostPort != port {
		t.Fatalf("concurrent adds must collapse to one map, store: %+v", maps)
	}

	// Mixed removes of the same port: all succeed or exactly one fails
	// with not-found; the store ends empty with the relay released.
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port, Proto: "tcp"})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h(context.Background(), &agent.Frame{ID: "r", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})
		}()
	}
	wg.Wait()
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("store after concurrent removes: %+v", got)
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("relay must be released after removes")
	}
}

// A push that really blocks until the request context fires must
// unblock at the deadline, roll back with a fresh bounded context, and
// release mapMu for the next operation.
func TestGuestMapRemoveBlockingPushCancelReleasesLock(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port, 80, "tcp"); err != nil {
		t.Fatal(err)
	}

	pushes := 0
	push := func(ctx context.Context, clusterID int) error {
		pushes++
		if pushes > 1 {
			// The rollback re-push gets a fresh bounded context and must
			// be allowed to complete.
			return nil
		}
		<-ctx.Done() // a genuinely stuck push, as a hung ssh would be
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port, Proto: "tcp"})
	start := time.Now()
	_, err := s.guestMapRemove(ctx, "dev1", rmReq, push)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("remove = %v, want rolled-back error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the deadline did not unblock the push")
	}
	if pushes != 2 {
		t.Fatalf("push calls = %d, want blocking push + rollback re-push", pushes)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 || got[0].HostPort != port {
		t.Fatalf("store must be restored: %+v", got)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("relay must be restored")
	}

	// mapMu was released: the next map operation completes promptly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mapMu.Lock(context.Background())
		defer s.mapMu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mapMu still held after the cancelled push")
	}
}

// The per-cluster gateway push gate must be abortable while waiting.
func TestRenderGatewayConfigGateCtxCancel(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	hold := make(chan struct{})
	go func() {
		_ = s.renderGatewayConfigLocked(context.Background(), 1, func([]byte) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.renderGatewayConfigLocked(ctx, 1, func([]byte) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate wait = %v, want context.DeadlineExceeded", err)
	}
	close(hold)
}

// The reviewer's interleaving: a guest remove of dev1's map racing the
// real host HTTP paths (remove + reassign attempt to dev2) must never
// leave two maps on one port or a relay that contradicts the store.
func TestGuestRemoveInterleavesWithHostMapPaths(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	h := s.guestControlHandlerWithPush("dev1", okPush)
	port := freePort(t, "tcp")

	for i := 0; i < 20; i++ {
		if _, _, err := s.addMap(context.Background(), dev1, port, 80, "tcp", s.relays.Ensure, okPush); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rmReq, _ := json.Marshal(agent.MapRemoveRequest{HostPort: port, Proto: "tcp"})
			h(context.Background(), &agent.Frame{ID: "r", Op: agent.OpControl, Action: agent.ControlMapRemove, Payload: rmReq})
		}()
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("DELETE", "/v1/maps/x/tcp", nil)
			req.SetPathValue("port", fmt.Sprint(port))
			req.SetPathValue("proto", "tcp")
			s.handleMapRemove(httptest.NewRecorder(), req)
			body := fmt.Sprintf(`{"vm":"dev2","host_port":%d,"guest_port":80,"proto":"tcp"}`, port)
			s.handleMapAdd(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/maps", strings.NewReader(body)))
		}()
		wg.Wait()

		var onPort []*cluster.PortMap
		for _, cl := range s.Clusters.List() {
			for _, m := range cl.Maps {
				if m.HostPort == port && m.Proto == "tcp" {
					onPort = append(onPort, m)
				}
			}
		}
		if len(onPort) > 1 {
			t.Fatalf("iter %d: %d maps on port %d", i, len(onPort), port)
		}
		if len(onPort) == 1 {
			if err := portFree("tcp", port); err == nil {
				t.Fatalf("iter %d: map without relay", i)
			}
		} else if err := portFree("tcp", port); err != nil {
			t.Fatalf("iter %d: relay outlives the map", i)
		}
	}
}

// Recovery must restore relays from the latest store, not a snapshot
// taken before the lock: a map removed after the snapshot but before
// the restore must stay unbound, or its port later forwards another
// VM's traffic.
func TestRecoverRelaysUsesLatestStore(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	portKept := freePort(t, "tcp")
	portRemoved := freePort(t, "tcp")
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, portKept, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, portRemoved, 81, "tcp"); err != nil {
		t.Fatal(err)
	}

	// recovery is blocked on mapMu; the remove commits first (store
	// mutex only), so the restore must observe it.
	s.mapMu.Lock(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.recoverRelays()
	}()
	if _, err := s.Clusters.RemoveMap(dev1.ClusterID, portRemoved, "tcp"); err != nil {
		t.Fatal(err)
	}
	s.mapMu.Unlock()
	<-done

	if err := portFree("tcp", portRemoved); err != nil {
		t.Fatal("recover re-bound a removed map's port")
	}
	if err := portFree("tcp", portKept); err == nil {
		t.Fatal("recover failed to bind a live map's port")
	}
}

// addMap must re-verify cluster membership so a delete/rollback that
// already removed the VM cannot be re-entered by a late map.
func TestAddMapRequiresClusterMembership(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	s.mapMu.Lock(context.Background())
	s.Clusters.RemoveVM(dev1.ClusterID, "dev1")
	s.mapMu.Unlock()

	_, code, err := s.addMap(context.Background(), dev1, port, 80, "tcp", s.relays.Ensure, okPush)
	if err == nil || code != 404 || !strings.Contains(err.Error(), "not a cluster member") {
		t.Fatalf("add for removed vm = %d %v, want 404 membership", code, err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("no map may be created for a removed vm: %+v", got)
	}
}

// Relay cleanup for a removed VM must read the latest store: a map
// committed after the caller's snapshot still gets its relay removed.
func TestRemoveVMMapsLockedUsesLatestStore(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port1 := freePort(t, "tcp")
	port2 := freePort(t, "tcp")
	m1, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port1, 80, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.relays.Ensure("tcp", port1, m1.PoolPort); err != nil {
		t.Fatal(err)
	}
	// The caller's snapshot predates the second map.
	cl := s.Clusters.Get(dev1.ClusterID)
	m2, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port2, 81, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.relays.Ensure("tcp", port2, m2.PoolPort); err != nil {
		t.Fatal(err)
	}

	s.mapMu.Lock(context.Background())
	s.removeVMMapsLocked(cl.ID, "dev1")
	s.mapMu.Unlock()

	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("maps remain after removal: %+v", got)
	}
	if err := portFree("tcp", port1); err != nil {
		t.Fatal("relay for the snapshot map survives")
	}
	if err := portFree("tcp", port2); err != nil {
		t.Fatal("relay for the post-snapshot map survives")
	}
}

// A real host delete racing a map add must leave no map and no relay
// for the deleted VM, in either order.
func TestDeleteInterleavesWithAdd(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	for i := 0; i < 10; i++ {
		if _, _, err := s.addMap(context.Background(), dev1, port, 80, "tcp", s.relays.Ensure, okPush); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("DELETE", "/v1/vms/dev1", nil)
			req.SetPathValue("name", "dev1")
			s.handleDelete(httptest.NewRecorder(), req)
		}()
		go func() {
			defer wg.Done()
			s.addMap(context.Background(), dev1, port, 80, "tcp", s.relays.Ensure, okPush)
		}()
		wg.Wait()

		for _, cl := range s.Clusters.List() {
			for _, m := range cl.Maps {
				if m.VM == "dev1" {
					t.Fatalf("iter %d: map for deleted vm survives: %+v", i, m)
				}
			}
			if cl.VMs["dev1"] != nil {
				t.Fatalf("iter %d: deleted vm still a member", i)
			}
		}
		if err := portFree("tcp", port); err != nil {
			t.Fatalf("iter %d: relay for deleted vm's map survives", i)
		}

		// Recreate dev1 for the next round.
		vnet, err := s.Clusters.AddVM(dev1.ClusterID, "dev1")
		if err != nil {
			t.Fatal(err)
		}
		dev1.IP = vnet.IP
		if err := os.MkdirAll(s.P.VMDir("dev1"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := dev1.Save(s.P); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.vms["dev1"] = dev1
		s.mu.Unlock()
	}
}

// pushBounded must give up on its own when the push gate never opens,
// so a hung ssh cannot hold mapMu forever.
func TestPushBoundedReleasesOnTimeout(t *testing.T) {
	s := newPushTestServer(t)
	cl, err := s.Clusters.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	gw := &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw"}
	if err := os.MkdirAll(s.P.VMDir(gw.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := gw.Save(s.P); err != nil {
		t.Fatal(err)
	}

	old := mapPushTimeout
	mapPushTimeout = 100 * time.Millisecond
	defer func() { mapPushTimeout = old }()

	started := make(chan struct{})
	hold := make(chan struct{})
	go func() {
		_ = s.renderGatewayConfigLocked(context.Background(), 1, func([]byte) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started
	begin := time.Now()
	err = s.pushBounded(1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pushBounded = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("pushBounded outlived its bound")
	}
	close(hold)
}

// desktop's map add must go through the shared membership check: a
// delete that already removed the VM blocks the desktop map.
func TestDesktopEnsureMapRequiresMembership(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	s.mapMu.Lock(context.Background())
	s.removeVMMapsLocked(dev1.ClusterID, "dev1")
	s.mapMu.Unlock()

	_, err := s.desktopEnsureMap(context.Background(), dev1, false, okPush)
	if err == nil || !strings.Contains(err.Error(), "not a cluster member") {
		t.Fatalf("desktop map for removed vm = %v, want membership error", err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("no map may be created for a removed vm: %+v", got)
	}
}

// A real delete racing the desktop map add must leave no map for the
// deleted VM, in either order.
func TestDesktopEnsureMapInterleavesWithDelete(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	for i := 0; i < 10; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("DELETE", "/v1/vms/dev1", nil)
			req.SetPathValue("name", "dev1")
			s.handleDelete(httptest.NewRecorder(), req)
		}()
		go func() {
			defer wg.Done()
			s.desktopEnsureMap(context.Background(), dev1, false, okPush)
		}()
		wg.Wait()

		for _, cl := range s.Clusters.List() {
			for _, m := range cl.Maps {
				if m.VM == "dev1" {
					t.Fatalf("iter %d: map for deleted vm survives: %+v", i, m)
				}
			}
		}
		if cl := s.Clusters.Get(dev1.ClusterID); cl.VMs["dev1"] != nil {
			t.Fatalf("iter %d: deleted vm still a member", i)
		}

		// Recreate dev1 for the next round.
		vnet, err := s.Clusters.AddVM(dev1.ClusterID, "dev1")
		if err != nil {
			t.Fatal(err)
		}
		dev1.IP = vnet.IP
		if err := os.MkdirAll(s.P.VMDir("dev1"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := dev1.Save(s.P); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.vms["dev1"] = dev1
		s.mu.Unlock()
	}
}

// clone inherit-ports shares the global port/proto ownership check: a
// port persisted by another cluster without a live relay cannot be
// inherited either.
func TestCloneInheritMapsCrossClusterPersistedPortRejected(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	cl2, err := s.Clusters.Ensure(2)
	if err != nil {
		t.Fatal(err)
	}
	vnet, err := s.Clusters.AddVM(cl2.ID, "dev9")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t, "tcp")
	if _, err := s.Clusters.AddMap(cl2.ID, "dev9", vnet.IP, port, 80, "tcp"); err != nil {
		t.Fatal(err)
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("test needs the port unbound on the host")
	}

	_, code, err := s.cloneInheritMaps(context.Background(), dev1, []cloneMapRequest{{HostPort: port, GuestPort: 80, Proto: "tcp"}})
	if err == nil || code != 409 || !strings.Contains(err.Error(), "already mapped") {
		t.Fatalf("clone inherit on persisted port = %d %v, want 409", code, err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("no map may be created in cluster 1: %+v", got)
	}
}

// The real host add path must give up on a gateway push that never
// completes: the bounded context fires, the add rolls back, and mapMu
// is released.
func TestHandleMapAddBlockingPushBounded(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	cl := s.Clusters.Get(dev1.ClusterID)
	gw := &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw"}
	if err := os.MkdirAll(s.P.VMDir(gw.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := gw.Save(s.P); err != nil {
		t.Fatal(err)
	}

	old := mapPushTimeout
	mapPushTimeout = 150 * time.Millisecond
	defer func() { mapPushTimeout = old }()

	port := freePort(t, "tcp")
	started := make(chan struct{})
	hold := make(chan struct{})
	go func() {
		_ = s.renderGatewayConfigLocked(context.Background(), cl.ID, func([]byte) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	body := fmt.Sprintf(`{"vm":"dev1","host_port":%d,"guest_port":80,"proto":"tcp"}`, port)
	rec := httptest.NewRecorder()
	begin := time.Now()
	s.handleMapAdd(rec, httptest.NewRequest("POST", "/v1/maps", strings.NewReader(body)))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "rolled back") {
		t.Fatalf("add = %d %s, want rolled-back 500", rec.Code, rec.Body.String())
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("the push deadline did not fire")
	}
	if got := s.Clusters.Get(cl.ID).Maps; len(got) != 0 {
		t.Fatalf("map must be rolled back: %+v", got)
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("relay must be removed by the rollback")
	}
	close(hold)
}

// Same for the real host remove path: a push that never completes
// rolls the removal back and releases mapMu.
func TestHandleMapRemoveBlockingPushBounded(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	cl := s.Clusters.Get(dev1.ClusterID)
	gw := &vm.Spec{Name: cl.Gateway, Role: "gateway", User: "pvmgw"}
	if err := os.MkdirAll(s.P.VMDir(gw.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := gw.Save(s.P); err != nil {
		t.Fatal(err)
	}
	port := freePort(t, "tcp")
	if _, _, err := s.addMap(context.Background(), dev1, port, 80, "tcp", s.relays.Ensure, okPush); err != nil {
		t.Fatal(err)
	}

	old := mapPushTimeout
	mapPushTimeout = 150 * time.Millisecond
	defer func() { mapPushTimeout = old }()

	started := make(chan struct{})
	hold := make(chan struct{})
	go func() {
		_ = s.renderGatewayConfigLocked(context.Background(), cl.ID, func([]byte) error {
			close(started)
			<-hold
			return nil
		})
	}()
	<-started

	req := httptest.NewRequest("DELETE", "/v1/maps/x/tcp", nil)
	req.SetPathValue("port", fmt.Sprint(port))
	req.SetPathValue("proto", "tcp")
	rec := httptest.NewRecorder()
	begin := time.Now()
	s.handleMapRemove(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "rolled back") {
		t.Fatalf("remove = %d %s, want rolled-back 500", rec.Code, rec.Body.String())
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("the push deadline did not fire")
	}
	if got := s.Clusters.Get(cl.ID).Maps; len(got) != 1 || got[0].HostPort != port {
		t.Fatalf("map must be restored by the rollback: %+v", got)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("relay must be restored by the rollback")
	}
	close(hold)
}

// The guest handler's mapMu wait itself must be cancellable: a guest
// that gives up at its deadline does not stay queued on the gate.
func TestGuestMapLockWaitCancelled(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	h := s.guestControlHandlerWithPush("dev1", okPush)

	s.mapMu.Lock(context.Background()) // hold the gate
	defer s.mapMu.Unlock()

	addReq, _ := json.Marshal(agent.MapAddRequest{HostPort: freePort(t, "tcp"), GuestPort: 80, Proto: "tcp"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h(ctx, &agent.Frame{ID: "a1", Op: agent.OpControl, Action: agent.ControlMapAdd, Payload: addReq})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the lock wait was not cancelled at the deadline")
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 0 {
		t.Fatalf("no map may be created: %+v", got)
	}
}

// A clone rollback that cannot get the gate (main push and rollback
// push both blocked on it) must NOT fall through to a destructive
// delete: the spec, membership, maps and relays stay recoverable, and
// a later real delete finishes the cleanup.
func TestRollbackCloneVMLockTimeoutPreservesState(t *testing.T) {
	s, _, dev2 := newMapTestServer(t)
	port := freePort(t, "tcp")
	m, err := s.Clusters.AddMap(dev2.ClusterID, "dev2", dev2.IP, port, 80, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.relays.Ensure("tcp", port, m.PoolPort); err != nil {
		t.Fatal(err)
	}

	old := rollbackLockTimeout
	rollbackLockTimeout = 200 * time.Millisecond
	defer func() { rollbackLockTimeout = old }()

	// Hold the gate: every map operation (and every push under it)
	// queues behind it.
	s.mapMu.Lock(context.Background())

	err = s.rollbackCloneVM(s.Clusters.Get(dev2.ClusterID), dev2)
	if err == nil || !strings.Contains(err.Error(), "pvm rm dev2") {
		t.Fatalf("rollback = %v, want recoverable pvm rm error", err)
	}
	// Nothing was deleted: spec, membership, map and relay all survive.
	if _, err := os.Stat(s.P.VMDir("dev2")); err != nil {
		t.Fatal("spec dir must survive a lock timeout")
	}
	if cl := s.Clusters.Get(dev2.ClusterID); cl.VMs["dev2"] == nil || len(cl.Maps) != 1 {
		t.Fatalf("membership and maps must survive: %+v", cl)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("relay must survive a lock timeout")
	}
	s.mapMu.Unlock()

	// The recovery path: a real delete finishes the precise cleanup.
	req := httptest.NewRequest("DELETE", "/v1/vms/dev2", nil)
	req.SetPathValue("name", "dev2")
	s.handleDelete(httptest.NewRecorder(), req)
	if _, err := os.Stat(s.P.VMDir("dev2")); err == nil {
		t.Fatal("delete must remove the spec dir once the gate is free")
	}
	if cl := s.Clusters.Get(dev2.ClusterID); cl.VMs["dev2"] != nil || len(cl.Maps) != 0 {
		t.Fatalf("membership and maps must be cleaned: %+v", cl)
	}
	if err := portFree("tcp", port); err != nil {
		t.Fatal("relay must be removed by the recovery delete")
	}
}

// Same guarantee for the create-start rollback: a lock timeout
// preserves everything and the error says how to finish.
func TestRollbackFailedCreateLockTimeoutPreservesState(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	port := freePort(t, "tcp")
	m, err := s.Clusters.AddMap(dev1.ClusterID, "dev1", dev1.IP, port, 80, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.relays.Ensure("tcp", port, m.PoolPort); err != nil {
		t.Fatal(err)
	}

	old := rollbackLockTimeout
	rollbackLockTimeout = 200 * time.Millisecond
	defer func() { rollbackLockTimeout = old }()

	s.mapMu.Lock(context.Background())

	err = s.rollbackFailedCreate(dev1, s.Clusters.Get(dev1.ClusterID))
	if err == nil || !strings.Contains(err.Error(), "pvm rm dev1") {
		t.Fatalf("rollback = %v, want recoverable pvm rm error", err)
	}
	if _, err := os.Stat(s.P.VMDir("dev1")); err != nil {
		t.Fatal("spec dir must survive a lock timeout")
	}
	if cl := s.Clusters.Get(dev1.ClusterID); cl.VMs["dev1"] == nil || len(cl.Maps) != 1 {
		t.Fatalf("membership and maps must survive: %+v", cl)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("relay must survive a lock timeout")
	}
	s.mapMu.Unlock()
}

// A desktop session start slower than mapPushTimeout must not fail the
// map operation: the ensure context is created after the start.
func TestDesktopEnsureMapFreshDeadlineAfterSlowStart(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	old := mapPushTimeout
	mapPushTimeout = 100 * time.Millisecond
	defer func() { mapPushTimeout = old }()

	// A context created before the slow session start is expired by the
	// time the ensure runs.
	staleCtx, staleCancel := context.WithTimeout(context.Background(), mapPushTimeout)
	defer staleCancel()
	time.Sleep(200 * time.Millisecond)
	if _, err := s.desktopEnsureMap(staleCtx, dev1, false, okPush); err == nil {
		t.Fatal("an expired context must fail, not succeed")
	}
	// The fresh context created after the start succeeds.
	freshCtx, freshCancel := context.WithTimeout(context.Background(), mapPushTimeout)
	defer freshCancel()
	port, err := s.desktopEnsureMap(freshCtx, dev1, false, okPush)
	if err != nil || port == 0 {
		t.Fatalf("fresh deadline after slow start: port=%d err=%v", port, err)
	}
	if got := s.Clusters.Get(dev1.ClusterID).Maps; len(got) != 1 || got[0].GuestPort != desktopWebPort {
		t.Fatalf("desktop map must exist: %+v", got)
	}
	if err := portFree("tcp", port); err == nil {
		t.Fatal("desktop relay must be bound")
	}
}

// The clone API keeps its explicit host port range: 0 is rejected even
// though plain map add auto-assigns it.
func TestCloneInheritMapsHostPortValidation(t *testing.T) {
	s, dev1, _ := newMapTestServer(t)
	if _, code, err := s.cloneInheritMaps(context.Background(), dev1, []cloneMapRequest{{HostPort: 0, GuestPort: 80, Proto: "tcp"}}); code != 400 ||
		!strings.Contains(err.Error(), "1024-65535") {
		t.Fatalf("host_port 0 = %d %v, want 400 range error", code, err)
	}
	if _, code, err := s.cloneInheritMaps(context.Background(), dev1, []cloneMapRequest{{HostPort: 80, GuestPort: 80, Proto: "tcp"}}); code != 400 {
		t.Fatalf("host_port 80 = %d %v, want 400", code, err)
	}
	// Plain map add still auto-assigns host_port 0.
	m, _, err := s.addMap(context.Background(), dev1, 0, 80, "tcp", s.relays.Ensure, okPush)
	if err != nil || m.HostPort == 0 {
		t.Fatalf("plain add with host_port 0: %v %v", m, err)
	}
}
