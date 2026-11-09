package cli

import "testing"

func TestPrepareDaemonForRuntimeChange(t *testing.T) {
	stops := 0
	stop := func() int { stops++; return 0 }
	for _, tc := range []struct {
		name                            string
		goos                            string
		daemonRunning, anyRunning       bool
		noRestart, qemuChange, wantStop bool
		wantError                       bool
	}{
		{name: "unix keeps existing order", goos: "linux", daemonRunning: true, anyRunning: true, qemuChange: true},
		{name: "windows stops before replacement", goos: "windows", daemonRunning: true, anyRunning: true, qemuChange: true, wantStop: true},
		{name: "windows binary only no restart", goos: "windows", daemonRunning: true, anyRunning: true, noRestart: true},
		{name: "windows running qemu no restart rejected", goos: "windows", daemonRunning: true, anyRunning: true, noRestart: true, qemuChange: true, wantError: true},
		{name: "windows idle no restart", goos: "windows", daemonRunning: true, noRestart: true, qemuChange: true},
		{name: "daemon already stopped", goos: "windows", qemuChange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stops = 0
			err := prepareDaemonForRuntimeChange(tc.goos, tc.daemonRunning, tc.anyRunning, tc.noRestart, tc.qemuChange, stop)
			if (err != nil) != tc.wantError {
				t.Fatalf("err = %v, wantError = %v", err, tc.wantError)
			}
			if (stops != 0) != tc.wantStop {
				t.Fatalf("stop calls = %d, wantStop = %v", stops, tc.wantStop)
			}
		})
	}
}
