package exit

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Suggested timezone detection for the exit-agent: the goal is the IANA
// zone matching the VPS's *public* geography, so clients routed via this
// exit can align their environment.
//
// Order: ipinfo.io (cached to <stateDir>/tz.json) -> cache -> system
// timezone -> "" (client treats as UTC).

var (
	ipinfoURL  = "https://ipinfo.io/json"
	httpClient = &http.Client{Timeout: 5 * time.Second}
	// stubs for tests
	ipinfoLookup = defaultIPInfoLookup
	systemTZFn   = systemTZ
)

// DetectSuggestedTZ resolves the suggested IANA timezone once. Called at
// exit-agent startup; the result is served on every enroll.
func DetectSuggestedTZ(stateDir string) string {
	cachePath := filepath.Join(stateDir, "tz.json")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if tz, err := ipinfoLookup(ctx); err == nil && tz != "" {
		_ = os.MkdirAll(stateDir, 0o700)
		b, _ := json.Marshal(map[string]string{"tz": tz})
		_ = os.WriteFile(cachePath, b, 0o600)
		return tz
	}
	if tz := tzFromCache(cachePath); tz != "" {
		return tz
	}
	return systemTZFn()
}

func defaultIPInfoLookup(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", ipinfoURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		Timezone string `json:"timezone"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Timezone, nil
}

func tzFromCache(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v struct {
		TZ string `json:"tz"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ""
	}
	return v.TZ
}

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
