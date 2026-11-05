package image

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/packet"
)

func testPublisher(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	entity, err := openpgp.NewEntity("PVM test publisher", "", "dev@example.com", &packet.Config{RSABits: 2048, DefaultHash: crypto.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	if err := entity.Serialize(&public); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "publisher.gpg")
	if err := os.WriteFile(path, public.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return entity, path
}

func TestFetchTrustedSignatureAndCachedFailures(t *testing.T) {
	if _, err := exec.LookPath("gpgv"); err != nil {
		t.Skip("gpgv is required for real signature verification")
	}
	signer, keyring := testPublisher(t)
	_, wrongKeyring := testPublisher(t)
	ambient := t.TempDir()
	public, err := os.ReadFile(keyring)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambient, "trustedkeys.gpg"), public, 0600); err != nil {
		t.Fatal(err)
	}
	// Even an ambient keyring with the signer must not expand explicit trust.
	t.Setenv("GNUPGHOME", ambient)
	payload := []byte("test image bytes")
	sums := []byte(fmt.Sprintf("%x  image.qcow2\n", sha256.Sum256(payload)))
	var signed bytes.Buffer
	if err := openpgp.DetachSign(&signed, signer, bytes.NewReader(sums), &packet.Config{DefaultHash: crypto.SHA256}); err != nil {
		t.Fatal(err)
	}
	signature := append([]byte(nil), signed.Bytes()...)
	var imageRequests atomic.Int32
	signatureStatus := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image":
			imageRequests.Add(1)
			w.Write(payload)
		case "/sums":
			w.Write(sums)
		case "/sig":
			w.WriteHeader(signatureStatus)
			w.Write(signature)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ref := &Ref{Distro: "ubuntu", Version: "24.04", Arch: "arm64", URL: server.URL + "/image", SumsURL: server.URL + "/sums", SignatureURL: server.URL + "/sig", SumFile: "image.qcow2", SumKind: "sha256", TrustedKeyring: keyring}
	dir := t.TempDir()
	final, err := Fetch(ref, dir, io.Discard)
	if err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	if _, err := Fetch(ref, dir, io.Discard); err != nil {
		t.Fatalf("valid cached signature: %v", err)
	}
	if imageRequests.Load() != 1 {
		t.Fatal("valid cache was downloaded again")
	}
	for _, tc := range []struct {
		name  string
		setup func()
		reset func()
	}{
		{"untrusted signer", func() { ref.TrustedKeyring = wrongKeyring }, func() { ref.TrustedKeyring = keyring }},
		{"tampered checksums", func() { sums = append(sums, '\n') }, func() { sums = sums[:len(sums)-1] }},
		{"bad signature", func() { signature = []byte("invalid") }, func() { signature = signed.Bytes() }},
		{"oversized signature", func() { signature = bytes.Repeat([]byte("x"), (1<<20)+1) }, func() { signature = signed.Bytes() }},
		{"missing signature", func() { signatureStatus = http.StatusNotFound }, func() { signatureStatus = http.StatusOK }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			defer tc.reset()
			if _, err := Fetch(ref, dir, io.Discard); err == nil {
				t.Fatal("cached image accepted without a trusted signature")
			}
			got, err := os.ReadFile(final)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("verification failure changed existing cache: %v", err)
			}
			if imageRequests.Load() != 1 {
				t.Fatal("verification failure fell back to a download")
			}
		})
	}
	signature = []byte("invalid")
	fresh := t.TempDir()
	if _, err := Fetch(ref, fresh, io.Discard); err == nil {
		t.Fatal("fresh image accepted without valid signature")
	}
	if _, err := os.Stat(filepath.Join(fresh, ref.FileName())); !os.IsNotExist(err) {
		t.Fatalf("unverified image published: %v", err)
	}
}

func TestSignaturePreflightFailsBeforeDownload(t *testing.T) {
	_, keyring := testPublisher(t)
	t.Setenv("PATH", t.TempDir())
	ref := &Ref{Distro: "ubuntu", Version: "24.04", Arch: "arm64", URL: "http://unused.invalid/image", SignatureURL: "http://unused.invalid/sig", TrustedKeyring: keyring}
	if _, err := Fetch(ref, t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "requires gpgv") {
		t.Fatalf("missing verifier: %v", err)
	}
	ref.TrustedKeyring = filepath.Join(t.TempDir(), "missing.gpg")
	if _, err := Fetch(ref, t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "trusted keyring") {
		t.Fatalf("missing keyring: %v", err)
	}
	ref.TrustedKeyring = keyring
	ref.SignatureURL = ""
	if _, err := Fetch(ref, t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "signature URL") {
		t.Fatalf("missing signature URL: %v", err)
	}
	ref.TrustedKeyring = ""
	if err := verifyChecksumSignature(ref, nil); err != nil {
		t.Fatalf("default mode requires verifier: %v", err)
	}
}

func TestResolvePublisherSignatureLocations(t *testing.T) {
	for _, tc := range []struct{ spec, suffix string }{{"ubuntu:24.04", "SHA256SUMS.gpg"}, {"debian:13", "SHA512SUMS.sign"}} {
		r, err := Resolve(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(r.SignatureURL, "/"+tc.suffix) || r.TrustedKeyring != "" {
			t.Fatalf("unexpected signature trust config: %+v", r)
		}
	}
}
