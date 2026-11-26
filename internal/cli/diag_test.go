package cli

import "testing"

func TestParseDiagArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		out     string
		offline bool
		wantErr bool
	}{
		{nil, "", false, false},
		{[]string{"--offline"}, "", true, false},
		{[]string{"out.zip"}, "out.zip", false, false},
		{[]string{"--offline", "out.zip"}, "out.zip", true, false},
		{[]string{"out.zip", "--offline"}, "out.zip", true, false},
		{[]string{"a.zip", "b.zip"}, "", false, true},
		{[]string{"--bogus"}, "", false, true},
	} {
		out, offline, err := parseDiagArgs(tc.args)
		if (err != nil) != tc.wantErr || out != tc.out || offline != tc.offline {
			t.Errorf("parseDiagArgs(%v) = %q, %v, %v; want %q, %v, err=%v", tc.args, out, offline, err, tc.out, tc.offline, tc.wantErr)
		}
	}
}
