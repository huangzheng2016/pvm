package cli

import (
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

func TestUpgradeRefusesUnresponsiveRunningDaemon(t *testing.T) {
	p := paths.New(filepath.Join(t.TempDir(), "state"))
	t.Setenv("PVM_HOME", p.Home)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.TryLock(p.DaemonRunLock())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if _, running, _, err := upgradeRunningVMs(); err == nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
}
