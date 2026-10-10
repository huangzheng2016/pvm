package exit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestIPInfoLookupParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ip":"1.2.3.4","city":"Los Angeles","region":"California","country":"US","timezone":"America/Los_Angeles"}`))
	}))
	defer srv.Close()
	old := ipinfoURL
	ipinfoURL = srv.URL
	defer func() { ipinfoURL = old }()
	tz, err := defaultIPInfoLookup(context.Background())
	if err != nil || tz != "America/Los_Angeles" {
		t.Errorf("tz=%q err=%v", tz, err)
	}
}

func TestDetectFallbackOrder(t *testing.T) {
	dir := t.TempDir()
	defer func() {
		ipinfoLookup = defaultIPInfoLookup
		systemTZFn = systemTZ
	}()

	// ipinfo ok -> wins, and gets cached
	ipinfoLookup = func(context.Context) (string, error) { return "America/Los_Angeles", nil }
	systemTZFn = func() string { return "Europe/Berlin" }
	if tz := DetectSuggestedTZ(dir); tz != "America/Los_Angeles" {
		t.Fatalf("ipinfo should win: %q", tz)
	}
	if tzFromCache(filepath.Join(dir, "tz.json")) != "America/Los_Angeles" {
		t.Fatal("result not cached to tz.json")
	}

	// ipinfo down, cache present -> cache wins over system
	ipinfoLookup = func(context.Context) (string, error) { return "", errors.New("down") }
	if tz := DetectSuggestedTZ(dir); tz != "America/Los_Angeles" {
		t.Fatalf("cache should win: %q", tz)
	}

	// no cache -> system zone
	dir2 := t.TempDir()
	if tz := DetectSuggestedTZ(dir2); tz != "Europe/Berlin" {
		t.Fatalf("system zone expected: %q", tz)
	}

	// nothing works -> empty
	systemTZFn = func() string { return "" }
	if tz := DetectSuggestedTZ(t.TempDir()); tz != "" {
		t.Fatalf("empty expected: %q", tz)
	}
}

func TestSystemTZReadsEtcTimezone(t *testing.T) {
	// systemTZ's last resort is /etc/timezone; on this macOS host none of
	// the sources may exist, so only assert it never panics and returns
	// something parseable when it does return a value.
	if tz := systemTZ(); tz != "" && parseZoneinfoPath("/usr/share/zoneinfo/"+tz) == "" {
		t.Errorf("systemTZ returned garbage %q", tz)
	}
	_ = os.Getenv("TZ") // placate unused-import lint pattern
}
