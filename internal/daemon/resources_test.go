package daemon

import (
	"testing"
	"time"
)

func TestDecideBalloonPressureAndIdlePolicy(t *testing.T) {
	const gib = uint64(1) << 30
	st := newBalloonState(4 * gib)
	now := time.Now()

	// Idle for less than 30s: no shrink yet.
	if got := decideBalloon(now, st, balloonSample{Available: 3 * gib, ProbeOK: true, BootID: "b1"}); got != 4*gib {
		t.Fatalf("initial = %d", got)
	}
	if got := decideBalloon(now.Add(10*time.Second), st, balloonSample{Available: 3 * gib, ProbeOK: true, BootID: "b1"}); got != 4*gib {
		t.Fatalf("early shrink = %d", got)
	}
	// Idle past 30s: reclaim at most 1/4 of excess (min 256 MiB).
	got := decideBalloon(now.Add(41*time.Second), st, balloonSample{Available: 3 * gib, ProbeOK: true, BootID: "b1"})
	if got >= 4*gib || got < 3*gib {
		t.Fatalf("idle reclaim out of bounds: %d", got)
	}
	// Pressure immediately returns toward the limit.
	st.current = 2 * gib
	if got := decideBalloon(now.Add(42*time.Second), st, balloonSample{Available: 100 << 20, PSISome: 2.5, ProbeOK: true, BootID: "b1"}); got != 3*gib {
		t.Fatalf("pressure grow = %d, want %d", got, 3*gib)
	}
	// OOM or probe failure restores the full limit immediately.
	st.current = 2 * gib
	if got := decideBalloon(now.Add(43*time.Second), st, balloonSample{Available: 3 * gib, OOMKills: 1, ProbeOK: true, BootID: "b1"}); got != 4*gib {
		t.Fatalf("oom restore = %d", got)
	}
	st.current = 2 * gib
	if got := decideBalloon(now.Add(44*time.Second), st, balloonSample{ProbeOK: false, BootID: "b1"}); got != 4*gib {
		t.Fatalf("probe failure restore = %d", got)
	}
}
