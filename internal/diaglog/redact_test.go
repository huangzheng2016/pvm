package diaglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactPatternsAndIdempotence(t *testing.T) {
	in := strings.Join([]string{
		"\x1b[31mERROR\x1b[0m Authorization: Bearer supersecrettoken123",
		"connect http://YOUR_TOKEN:pass@proxy.example.com:8080 failed",
		"url http://127.0.0.1:6080/vnc.html?password=deskpass123&resize=scale",
		`{"token":"YOUR_TOKEN","private_key":"wG0secretKeyMaterial000000000000000000000=","public_key":"keepPubKey0000="}`,
		"api request with ghp_abcdefghijklmnop123456 and sk-abcdefghijklmnop1234",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----",
		"path /Users/alice/project and /home/bob/.pvm and C:\\Users\\carol\\work",
		"exit 198.51.100.99 and 8.8.4.4 vs private 10.0.2.2 100.99.1.5 192.168.1.1 doc 203.0.113.5",
		"-----BEGIN PRIVATE KEY-----\nunterminated-block",
	}, "\n")

	got := Redact(in)
	for _, forbidden := range []string{
		"\x1b[", "supersecrettoken123", "YOUR_TOKEN", "deskpass123", "wG0secretKeyMaterial",
		"ghp_abcdefghijklmnop", "sk-abcdefghijklmnop", "b3BlbnNzaC1rZXk", "unterminated-block",
		"/Users/alice", "/home/bob", `\Users\carol`, "8.8.4.4",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("redacted output still contains %q:\n%s", forbidden, got)
		}
	}
	for _, kept := range []string{
		"keepPubKey0000=", "proxy.example.com", "resize=scale",
		"/Users/[user]/project", "/home/[user]/.pvm", `C:\Users\[user]\work`,
		"10.0.2.2", "100.99.1.5", "192.168.1.1", "203.0.113.5", "198.51.100.99",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("redacted output lost diagnostic evidence %q:\n%s", kept, got)
		}
	}
	if again := Redact(got); again != got {
		t.Errorf("Redact is not idempotent:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

func TestRedactorExactSecretsAndWriter(t *testing.T) {
	r := NewRedactor()
	r.AddSecret("opaque-exit-token-xyz")
	r.AddEndpoint("vps.internal-example.test")
	r.AddPath("/srv/pvm-state", "[pvm-home]")
	var b strings.Builder
	w := NewRedactWriter(&b, r)
	if _, err := w.Write([]byte("dial vps.internal-example.test with opaque-exit-token-xyz in /srv/pvm-state/exits\n")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if strings.Contains(got, "vps.internal-example.test") || strings.Contains(got, "opaque-exit-token-xyz") || strings.Contains(got, "/srv/pvm-state") {
		t.Fatalf("exact secrets leaked: %s", got)
	}
	if !strings.Contains(got, "[host]") || !strings.Contains(got, "[pvm-home]/exits") {
		t.Fatalf("expected placeholders missing: %s", got)
	}
}

func TestRotateFileAndWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	if err := RotateFile(path, 10, 2); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RotateFile(path, 10, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("missing rotated file: %v", err)
	}

	w, err := NewRotateWriter(path, 32, 2, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := w.Write([]byte("0123456789abcdefghij\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > 32 {
		t.Fatalf("active log size = %v, %v; want <= 32", fi, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("missing writer rotation: %v", err)
	}
}
