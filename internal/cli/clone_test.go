package cli

import (
	"bufio"
	"strings"
	"testing"
)

func busySet(busy map[int]bool) func(string, int) bool {
	return func(_ string, p int) bool { return !busy[p] }
}

func TestPlanMapsNone(t *testing.T) {
	maps := []srcMapView{{HostPort: 18090, GuestPort: 80, Proto: "tcp"}}
	plans, err := planMaps(maps, "none", busySet(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Fatalf("none mode should skip all, got %v", plans)
	}
}

func TestPlanMapsInherit(t *testing.T) {
	maps := []srcMapView{
		{HostPort: 18090, GuestPort: 80, Proto: "tcp"},
		{HostPort: 18091, GuestPort: 443, Proto: "tcp"},
	}
	// 18091 busy: second map must remap to the next free port.
	plans, err := planMaps(maps, "inherit", busySet(map[int]bool{18091: true}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("want 2 plans, got %v", plans)
	}
	if plans[0].HostPort != 18090 || plans[0].Note != "" {
		t.Errorf("first map should keep 18090 without note: %+v", plans[0])
	}
	if plans[1].HostPort != 18092 || plans[1].Note == "" {
		t.Errorf("second map should remap 18091 -> 18092 with note: %+v", plans[1])
	}
	if plans[1].GuestPort != 443 {
		t.Errorf("guest port must follow source: %+v", plans[1])
	}
}

func TestPlanMapsInheritDefaultProto(t *testing.T) {
	maps := []srcMapView{{HostPort: 8080, GuestPort: 53}}
	plans, err := planMaps(maps, "inherit", busySet(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plans[0].Proto != "tcp" {
		t.Errorf("empty proto should default to tcp: %+v", plans[0])
	}
}

func TestPlanMapsInteractive(t *testing.T) {
	maps := []srcMapView{
		{HostPort: 18090, GuestPort: 80, Proto: "tcp"},
		{HostPort: 18091, GuestPort: 443, Proto: "tcp"},
		{HostPort: 18092, GuestPort: 22, Proto: "tcp"},
	}
	answers := []func(srcMapView, int) (int, bool){
		func(m srcMapView, _ int) (int, bool) { return m.HostPort, true }, // keep
		func(_ srcMapView, _ int) (int, bool) { return 0, false },         // skip
		func(_ srcMapView, _ int) (int, bool) { return 29922, true },      // custom
	}
	i := 0
	prompt := func(m srcMapView, suggested int) (int, bool) {
		f := answers[i]
		i++
		return f(m, suggested)
	}
	plans, err := planMaps(maps, "interactive", busySet(nil), prompt)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("want 2 plans (one skipped), got %v", plans)
	}
	if plans[0].HostPort != 18090 || plans[1].HostPort != 29922 {
		t.Errorf("wrong ports: %v", plans)
	}
}

func TestNextFreePort(t *testing.T) {
	free := busySet(map[int]bool{18091: true, 18092: true})
	if got := nextFreePort("tcp", 18091, free); got != 18093 {
		t.Errorf("nextFreePort(18091) = %d, want 18093", got)
	}
	// Everything from prefer up is busy: wrap around to low range.
	free2 := func(_ string, p int) bool { return p == 1024 }
	if got := nextFreePort("tcp", 65535, free2); got != 1024 {
		t.Errorf("wrap-around nextFreePort = %d, want 1024", got)
	}
	free3 := func(_ string, _ int) bool { return false }
	if got := nextFreePort("tcp", 18091, free3); got != 0 {
		t.Errorf("no free port should yield 0, got %d", got)
	}
}

func TestPromptMapInherit(t *testing.T) {
	m := srcMapView{HostPort: 18090, GuestPort: 80, Proto: "tcp"}
	free := busySet(nil)
	busy18090 := busySet(map[int]bool{18090: true})

	// Custom port.
	port, ok := promptMapInherit(bufio.NewReader(strings.NewReader("18095\n")), free)(m, 18091)
	if !ok || port != 18095 {
		t.Errorf("custom port: got (%d,%v)", port, ok)
	}
	// y with free original port keeps it.
	port, ok = promptMapInherit(bufio.NewReader(strings.NewReader("y\n")), free)(m, 18091)
	if !ok || port != 18090 {
		t.Errorf("y free: got (%d,%v)", port, ok)
	}
	// y with busy original port remaps to the suggested free one.
	port, ok = promptMapInherit(bufio.NewReader(strings.NewReader("y\n")), busy18090)(m, 18091)
	if !ok || port != 18091 {
		t.Errorf("y busy: got (%d,%v)", port, ok)
	}
	// Skip answers.
	for _, in := range []string{"\n", "n\n", "no\n", "abc\n", "80\n"} {
		if _, ok := promptMapInherit(bufio.NewReader(strings.NewReader(in)), free)(m, 18091); ok {
			t.Errorf("input %q should skip", in)
		}
	}
	// Custom busy port skips.
	if _, ok := promptMapInherit(bufio.NewReader(strings.NewReader("18090\n")), busy18090)(m, 18091); ok {
		t.Error("busy custom port should skip")
	}
	// EOF skips.
	if _, ok := promptMapInherit(bufio.NewReader(strings.NewReader("")), free)(m, 18091); ok {
		t.Error("EOF should skip")
	}
}

func TestPartitionFlags(t *testing.T) {
	got := partitionFlags([]string{"c1", "c4", "--inherit-ports", "--no-start"})
	want := []string{"--inherit-ports", "--no-start", "c1", "c4"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if got := partitionFlags([]string{"c1", "c2"}); len(got) != 2 || got[0] != "c1" {
		t.Fatalf("no flags: got %v", got)
	}
}
