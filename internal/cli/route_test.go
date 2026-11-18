package cli

import (
	"strings"
	"testing"
)

func TestVMRouteOutcome(t *testing.T) {
	for _, tc := range []struct {
		resp map[string]any
		want string
	}{
		{map[string]any{"restarted": true, "pushed": true}, "stopped and restarted"},
		{map[string]any{"restarted": false, "pushed": true}, "config pushed"},
		{map[string]any{"restarted": false, "pushed": false}, "route saved; no gateway is running, so it is applied when dev1 next starts."},
	} {
		got := vmRouteOutcome("dev1", tc.resp)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%v: got %q, want it to contain %q", tc.resp, got, tc.want)
		}
		if pushed, _ := tc.resp["pushed"].(bool); !pushed && strings.Contains(got, "pushed") {
			t.Errorf("%v: %q claims a push that did not happen", tc.resp, got)
		}
	}
}
