package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestWARPExitLifecycle(t *testing.T) {
	private, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := tunnel.PublicKey(private)
	if err != nil {
		t.Fatal(err)
	}
	registers, deletes := 0, 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0a5641/reg":
			registers++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":      "device-1",
				"token":   "device-token",
				"account": map[string]any{},
				"config": map[string]any{
					"interface": map[string]any{"addresses": map[string]any{"v4": "172.16.0.2", "v6": "2606:4700:110::2"}},
					"peers": []any{map[string]any{
						"public_key": public,
						"endpoint":   map[string]any{"host": "203.0.113.10:2408"},
					}},
				},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v0a5641/reg/device-1":
			deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	oldBase := warpAPIBase
	warpAPIBase = ts.URL
	t.Cleanup(func() { warpAPIBase = oldBase })
	home := t.TempDir()
	t.Setenv("PVM_HOME", home)

	if code := exitAddWARP([]string{"warp-test", "--tz", "UTC"}); code != 0 {
		t.Fatalf("add-warp exit code = %d", code)
	}
	if registers != 1 {
		t.Fatalf("register calls = %d", registers)
	}
	dir := filepath.Join(home, "exits")
	entry, err := exit.LoadEntry(dir, "warp-test")
	if err != nil {
		t.Fatal(err)
	}
	if !entry.IsWARP() || entry.Endpoint != "203.0.113.10:2408" || entry.TZ != "UTC" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if entry.WARP == nil || entry.WARP.Token != "device-token" {
		t.Fatal("device credentials were not saved")
	}
	info, err := os.Stat(filepath.Join(dir, "warp-test.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("entry mode = %o", info.Mode().Perm())
	}
	output := filepath.Join(t.TempDir(), "client.conf")
	if code := exitClientConfigTo([]string{"warp-test", "-o", output}, &bytes.Buffer{}); code == 0 {
		t.Fatal("WARP must not export the gateway's client key")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("unexpected exported config: %v", err)
	}
	if code := exitTest([]string{"warp-test", "--speed"}); code == 0 {
		t.Fatal("VPS-only speed test must be rejected for WARP")
	}
	if code := exitRm([]string{"warp-test"}); code != 0 {
		t.Fatalf("exit rm exit code = %d", code)
	}
	if deletes != 1 {
		t.Fatalf("delete calls = %d", deletes)
	}
	if _, err := exit.LoadEntry(dir, "warp-test"); err == nil {
		t.Fatal("local exit still exists")
	}
}
