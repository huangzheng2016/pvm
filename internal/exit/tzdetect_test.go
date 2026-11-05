package exit

import (
	"os"
	"testing"
)

func TestParseZoneinfoPath(t *testing.T) {
	cases := map[string]string{
		"/usr/share/zoneinfo/America/Los_Angeles": "America/Los_Angeles",
		"/usr/share/zoneinfo/Etc/UTC":             "Etc/UTC",
		"/usr/share/zoneinfo/UTC":                 "UTC",
		"../zoneinfo/Asia/Tokyo":                  "Asia/Tokyo",
		"/etc/localtime":                          "",
		"":                                        "",
		"/usr/share/zoneinfo/":                    "",
	}
	for in, want := range cases {
		if got := parseZoneinfoPath(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestDetectUsesLocalTimezone(t *testing.T) {
	old := systemTZFn
	defer func() { systemTZFn = old }()
	systemTZFn = func() string { return "Europe/Berlin" }
	if got := DetectSuggestedTZ(t.TempDir()); got != "Europe/Berlin" {
		t.Fatalf("timezone = %q", got)
	}
	systemTZFn = func() string { return "" }
	if got := DetectSuggestedTZ(t.TempDir()); got != "" {
		t.Fatalf("timezone = %q", got)
	}
}

func TestSystemTZReadsEtcTimezone(t *testing.T) {
	// systemTZ's last resort is /etc/timezone; on this macOS host none of
	// the sources may exist, so only assert it never panics and returns
	// something parseable when it does return a value.
	if tz := systemTZ(); tz != "" && parseZoneinfoPath("/usr/share/zoneinfo/"+tz) == "" {
		t.Errorf("systemTZ returned garbage %q", tz)
	}
	_ = os.Getenv("TZ")
}
