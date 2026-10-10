package cli

import (
	"strings"
	"testing"
)

func TestConfirmAction(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"Y\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"\n", false},
		{"no\n", false},
		{"whatever\n", false},
		{"", false}, // EOF
	}
	for _, c := range cases {
		var w strings.Builder
		got := confirmAction(strings.NewReader(c.in), &w, "delete?")
		if got != c.want {
			t.Errorf("confirmAction(%q) = %v, want %v", c.in, got, c.want)
		}
		if !strings.Contains(w.String(), "delete?") {
			t.Errorf("prompt not written: %q", w.String())
		}
	}
}
