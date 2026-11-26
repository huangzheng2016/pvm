package diaglog

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/paths"
)

func TestExportBundleWhitelistAndRedaction(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(filepath.Join(p.Home, "exits"), &exit.Entry{
		Name: "my-exit", Server: "198.51.100.7:18443", Endpoint: "198.51.100.7:51820",
		Token: "exit-secret-token-123456", WGPrivateKey: "wgSecretKey000000000000000000000000000000=",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DaemonAdminToken(), []byte("admin-secret-token-987654\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vmDir := p.VMDir("dev1")
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "desktop.json"), []byte(`{"password":"vnc-secret-pass-abcdef","host_port":6080}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "vm.json"), []byte(`{"name":"dev1","role":"user","user":"dev","image":"ubuntu:24.04","cpus":2,"mem_mb":2048,"mounts":[{"host":"/Users/alice/secret-project","guest":"/mnt/proj","profile":"coherent"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	console := "route via my-exit 198.51.100.7 token=exit-secret-token-123456 admin-secret-token-987654 password=vnc-secret-pass-abcdef key wgSecretKey000000000000000000000000000000= mount /Users/alice/secret-project\n"
	if err := os.WriteFile(filepath.Join(vmDir, "console.log"), []byte(console), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DaemonLog(), []byte(console), 0o600); err != nil {
		t.Fatal(err)
	}
	// A raw private key file inside the VM dir must never be swept into the bundle.
	if err := os.WriteFile(filepath.Join(vmDir, "id_ed25519"), []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "diag.zip")
	m, err := ExportBundle(p, out, func() (map[string][]byte, error) {
		return map[string][]byte{"daemon/status.json": []byte(`{"ready":true,"token":"exit-secret-token-123456"}`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != "pvm-diag/1" || m.Files == 0 {
		t.Fatalf("unexpected manifest: %+v", m)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var all strings.Builder
	for _, f := range zr.File {
		if strings.Contains(f.Name, "id_ed25519") || strings.Contains(f.Name, "desktop.json") || strings.HasSuffix(f.Name, "/vm.json") {
			t.Errorf("raw sensitive file %q included in bundle", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	bundle := all.String()
	for _, secret := range []string{"exit-secret-token-123456", "admin-secret-token-987654", "vnc-secret-pass-abcdef", "wgSecretKey0000", "/Users/alice"} {
		if strings.Contains(bundle, secret) {
			t.Errorf("bundle leaked %q:\n%s", secret, bundle)
		}
	}
	if !strings.Contains(bundle, `"guest": "/mnt/proj"`) || !strings.Contains(bundle, `"ready":true`) || strings.Count(bundle, `"guest": "/mnt/proj"`) != 2 {
		t.Errorf("bundle lost whitelist metadata:\n%s", bundle)
	}
}
