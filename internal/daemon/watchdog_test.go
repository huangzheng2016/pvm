package daemon

import "testing"

func TestDecideStall(t *testing.T) {
	cases := []struct {
		name string
		in   stallInput
		want stallAction
	}{
		{name: "high cpu no progress restarts once", in: stallInput{CPUPercent: 99.7}, want: stallRestart},
		{name: "ssh ready is healthy", in: stallInput{SSHReady: true, CPUPercent: 99.7}, want: stallContinue},
		{name: "console progressing is not stalled", in: stallInput{ConsoleProgress: true, CPUPercent: 99.7}, want: stallContinue},
		{name: "low cpu is not stalled", in: stallInput{CPUPercent: 20}, want: stallContinue},
		{name: "second stall only warns", in: stallInput{CPUPercent: 99.7, Attempts: 1}, want: stallWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideStall(tc.in); got != tc.want {
				t.Fatalf("decideStall(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseCPUPercent(t *testing.T) {
	v, err := parseCPUPercent("  99.7\n")
	if err != nil || v != 99.7 {
		t.Fatalf("parseCPUPercent = %v, %v", v, err)
	}
	if _, err := parseCPUPercent("not-a-number"); err == nil {
		t.Fatal("invalid cpu input should error")
	}
}
