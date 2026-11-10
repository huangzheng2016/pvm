package vm

import "testing"

func TestGuestUID(t *testing.T) {
	for _, tc := range []struct{ host, want int }{
		{-1, 1000},
		{0, 1000},
		{501, 501},
		{1000, 1000},
	} {
		if got := guestUID(tc.host); got != tc.want {
			t.Fatalf("guestUID(%d) = %d, want %d", tc.host, got, tc.want)
		}
	}
}
