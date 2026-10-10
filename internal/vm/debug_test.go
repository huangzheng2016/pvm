package vm

import "testing"

func TestDebugUserData(t *testing.T) {
	ud, err := userData(&Spec{Name: "a2", Image: "alpine:3.20", User: "tester"}, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB+GxOdVYmeO2m1vHemY1QaUyy449fL1LCqEKX4N6fb+", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(ud))
}
