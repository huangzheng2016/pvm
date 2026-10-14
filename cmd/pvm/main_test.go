package main

import "testing"

func TestHelpRequested(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"ssh", "-h"}, {"image", "fetch", "--help"}} {
		if !helpRequested(args) {
			t.Errorf("help not detected for %v", args)
		}
	}
	for _, args := range [][]string{nil, {"ssh", "u26"}, {"exec", "u26", "help"}, {"exec", "u26", "--", "--help"}} {
		if helpRequested(args) {
			t.Errorf("unexpected help detection for %v", args)
		}
	}
}
