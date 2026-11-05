package daemon

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

func TestSlowAgentDoesNotBlockAnotherVM(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pvm-dial-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := paths.New(dir)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.VMDir("dev1"), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.AgentSock("dev1"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotPing := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		close(gotPing)
		<-release
	}()
	s := NewServer(p, &config.Config{}, nil, qemu.AccelTCG, nil)
	slowCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := s.agentClientContext(slowCtx, "dev1"); finished <- err }()
	select {
	case <-gotPing:
	case <-time.After(time.Second):
		t.Fatal("slow ping not sent")
	}
	startFakeGuest(t, p.AgentSock("dev2"), nil)
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	c, err := s.agentClientContext(ctx, "dev2")
	if err != nil {
		t.Fatalf("healthy VM blocked: %v", err)
	}
	defer c.Close()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("unanswered ping succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("dial context did not cancel ping")
	}
}

func TestPendingCloneOffersRecoveryKeyOnlyUntilCommitted(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	spec.SSHKeyName = "source"
	spec.IdentityReset = true
	key := s.P.VMSSHKey(spec.Name)
	if err := os.MkdirAll(filepath.Dir(key), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	args, _, err := s.sshBaseArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-i "+spec.SSHKeyPath(s.P)) || !strings.Contains(joined, "-i "+key) {
		t.Fatal("pending reset cannot recover after guest key replacement")
	}
	spec.IdentityReset = false
	spec.SSHKeyName = spec.Name
	args, _, err = s.sshBaseArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, arg := range args {
		if arg == "-i" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("completed identity reset still offers bootstrap identity")
	}
}
