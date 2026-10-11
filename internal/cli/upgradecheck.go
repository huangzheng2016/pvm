package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
	"github.com/huangzheng2016/pvm/internal/paths"
)

type upgradeCheckCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// MaybeNotifyUpgrade prints a one-line hint to stderr when a newer release
// exists. Cached for 24h, silent on any error, PVM_NO_UPDATE_CHECK=1 disables.
func MaybeNotifyUpgrade() {
	if os.Getenv("PVM_NO_UPDATE_CHECK") == "1" || buildinfo.Version == "dev" {
		return
	}
	p, err := paths.Default()
	if err != nil {
		return
	}
	cacheFile := filepath.Join(p.Run, "version-check.json")
	var cache upgradeCheckCache
	if data, err := os.ReadFile(cacheFile); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	if time.Since(cache.CheckedAt) > 24*time.Hour {
		client := &http.Client{Timeout: 3 * time.Second}
		latest, err := latestReleaseTag(client)
		if err != nil {
			return
		}
		cache = upgradeCheckCache{CheckedAt: time.Now(), Latest: latest}
		if data, err := json.Marshal(cache); err == nil {
			_ = os.MkdirAll(p.Run, 0o700)
			_ = os.WriteFile(cacheFile, data, 0o600)
		}
	}
	if cache.Latest != "" && versionNewer(cache.Latest, buildinfo.Version) {
		fmt.Fprintf(os.Stderr, "pvm %s is available (current %s); run `pvm upgrade` to update\n", cache.Latest, buildinfo.Version)
	}
}

// versionNewer reports whether a is a newer semver than b (v-prefix and
// pre-release suffix tolerated; release outranks pre-release of the same triple).
func versionNewer(a, b string) bool {
	at, apre := parseVersion(a)
	bt, bpre := parseVersion(b)
	if at != bt {
		return at > bt
	}
	return bpre && !apre
}

func parseVersion(v string) (int, bool) {
	v = strings.TrimPrefix(v, "v")
	pre := false
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = true
		v = v[:i]
	}
	var n int
	for _, part := range strings.Split(v, ".") {
		num, err := strconv.Atoi(part)
		if err != nil {
			return 0, false
		}
		n = n*1000 + num
	}
	return n, pre
}
