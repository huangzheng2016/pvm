package vm

import "testing"

func TestValidateTZ(t *testing.T) {
	for _, ok := range []string{"America/Los_Angeles", "Etc/UTC", "Asia/Shanghai", "America/Argentina/Buenos_Aires", "UTC"} {
		if err := ValidateTZ(ok); err != nil {
			t.Errorf("%s should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "America/Los Angeles", "x;rm -rf /", "$(reboot)", "../etc", "a/b/c/d"} {
		if err := ValidateTZ(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestNormalizeLocale(t *testing.T) {
	cases := map[string]string{
		"en_US.UTF-8": "en_US.UTF-8",
		"en_US":       "en_US.UTF-8",
		"en_US.utf8":  "en_US.UTF-8",
		"zh_CN.UTF-8": "zh_CN.UTF-8",
		"de_DE.utf-8": "de_DE.UTF-8",
	}
	for in, want := range cases {
		got, err := NormalizeLocale(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "EN_us.UTF-8", "en_US.ISO-8859-1", "x';rm#/UTF-8", "en-US.UTF-8"} {
		if _, err := NormalizeLocale(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
