package exit

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DetectSuggestedTZ uses only local configuration. Starting an exit must not
// contact a third party merely to choose a timezone.
var systemTZFn = systemTZ

func DetectSuggestedTZ(_ string) string { return systemTZFn() }

// systemTZ resolves the VPS's own zone: /etc/localtime symlink, then
// timedatectl (bounded), then /etc/timezone text.
func systemTZ() string {
	if p, err := os.Readlink("/etc/localtime"); err == nil {
		if tz := parseZoneinfoPath(p); tz != "" {
			return tz
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "timedatectl", "show", "-p", "Timezone", "--value").Output(); err == nil {
		if tz := strings.TrimSpace(string(out)); tz != "" {
			return tz
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// parseZoneinfoPath extracts Region/City from a zoneinfo path, e.g.
// /usr/share/zoneinfo/America/Los_Angeles -> America/Los_Angeles.
func parseZoneinfoPath(p string) string {
	const marker = "/zoneinfo/"
	i := strings.LastIndex(p, marker)
	if i < 0 {
		return ""
	}
	tz := p[i+len(marker):]
	if tz == "" || strings.Contains(tz, "..") {
		return ""
	}
	return tz
}
