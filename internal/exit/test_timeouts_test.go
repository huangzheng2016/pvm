package exit

import "time"

// Test operation timeouts; relaxed under -race (see race_relax_test.go).
var (
	testOpTimeout    = 10 * time.Second
	testLongTimeout  = 45 * time.Second
	testProbeTimeout = 700 * time.Millisecond
)
