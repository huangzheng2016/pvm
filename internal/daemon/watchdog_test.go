package daemon

import "testing"

func TestDecideStall(t *testing.T) {
	cases := []struct {
		name string
		in   stallInput
		want stallAction
	}{
		{name: "high cpu no progress restarts once", in: stallInput{CPUPercent: 99.7, StallTicks: 2}, want: stallRestart},
		{name: "first quiet tick is not yet a stall", in: stallInput{CPUPercent: 99.7, StallTicks: 1}, want: stallContinue},
		{name: "ssh ready is healthy", in: stallInput{SSHReady: true, CPUPercent: 99.7, StallTicks: 2}, want: stallContinue},
		{name: "console progressing is not stalled", in: stallInput{ConsoleProgress: true, CPUPercent: 99.7, StallTicks: 2}, want: stallContinue},
		{name: "low cpu is not stalled", in: stallInput{CPUPercent: 20, StallTicks: 2}, want: stallContinue},
		{name: "second stall only warns", in: stallInput{CPUPercent: 99.7, Attempts: 1, StallTicks: 2}, want: stallWarn},
		{name: "second stall also needs two ticks", in: stallInput{CPUPercent: 99.7, Attempts: 1, StallTicks: 1}, want: stallContinue},
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
