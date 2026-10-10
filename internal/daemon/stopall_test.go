package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestStopInfosParallel(t *testing.T) {
	infos := make([]vmInfo, 4)
	for i := range infos {
		infos[i] = vmInfo{Spec: &vm.Spec{Name: string(rune('a' + i))}, Running: true}
	}

	var mu sync.Mutex
	var active, maxActive, calls int
	release := make(chan struct{})
	var releaseOnce sync.Once
	stop := func(*vm.Spec) {
		mu.Lock()
		active++
		calls++
		if active > maxActive {
			maxActive = active
		}
		if active == 2 {
			releaseOnce.Do(func() { close(release) })
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		stopInfosParallel(infos, 2, stop)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopInfosParallel did not run concurrently")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	if maxActive > 2 {
		t.Fatalf("maxActive = %d, want <= 2", maxActive)
	}
}
