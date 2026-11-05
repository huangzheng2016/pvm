package cli

import (
	"errors"
	"github.com/huangzheng2016/pvm/internal/buildinfo"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.1.1", "0.1.0-m1", true},
		{"v0.1.0", "0.1.0-m1", true}, // release outranks pre-release of same triple
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"0.1.0-m1", "v0.1.0", false},
		{"garbage", "v0.1.0", false},
	}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestUpgradeCheckFailureBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "version-check.json")
	now := time.Now()
	calls := 0
	fetch := func() (string, error) { calls++; return "", errors.New("offline") }
	refreshUpgradeCheck(path, now, fetch)
	refreshUpgradeCheck(path, now.Add(time.Hour), fetch)
	if calls != 1 {
		t.Fatalf("failed check retried immediately: %d", calls)
	}
	refreshUpgradeCheck(path, now.Add(25*time.Hour), fetch)
	if calls != 2 {
		t.Fatalf("failed check never retried: %d", calls)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("cache mode %o", st.Mode().Perm())
	}
}

func TestUpgradeCheckDefaultDoesNotConnect(t *testing.T) {
	t.Setenv("PVM_HOME", t.TempDir())
	t.Setenv("PVM_UPDATE_CHECK", "")
	t.Setenv("PVM_NO_UPDATE_CHECK", "")
	oldVersion := buildinfo.Version
	buildinfo.Version = "v1.0.0"
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	transport := http.DefaultTransport
	http.DefaultTransport = upgradeRejectTransport{t}
	t.Cleanup(func() { http.DefaultTransport = transport })
	MaybeNotifyUpgrade()
}

type upgradeRejectTransport struct{ t *testing.T }

func (r upgradeRejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("default update check unexpectedly contacted network")
	return nil, errors.New("network disabled")
}
