package image

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// signatureConfig deliberately does not discover or download trust anchors.
// Supplying a keyring opts into mandatory verification against its keys.
func signatureConfig(r *Ref) (binary, keyring string, err error) {
	if r.TrustedKeyring == "" {
		return "", "", nil
	}
	if r.SignatureURL == "" {
		return "", "", fmt.Errorf("signature verification requires a checksum signature URL")
	}
	keyring, err = filepath.Abs(r.TrustedKeyring)
	if err != nil {
		return "", "", fmt.Errorf("trusted keyring: %w", err)
	}
	st, err := os.Stat(keyring)
	if err != nil {
		return "", "", fmt.Errorf("trusted keyring: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", "", fmt.Errorf("trusted keyring must be a regular file")
	}
	binary, err = exec.LookPath("gpgv")
	if err != nil {
		return "", "", fmt.Errorf("signature verification requires gpgv: install GnuPG before using --keyring")
	}
	return binary, keyring, nil
}

func verifyChecksumSignature(r *Ref, sums []byte) error {
	binary, keyring, err := signatureConfig(r)
	if err != nil {
		return err
	}
	if keyring == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.SignatureURL, nil)
	if err != nil {
		return fmt.Errorf("checksum signature request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch checksum signature: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch checksum signature: %s", resp.Status)
	}
	const maxSignatureBytes = 1 << 20
	signature, err := io.ReadAll(io.LimitReader(resp.Body, maxSignatureBytes+1))
	if err != nil {
		return fmt.Errorf("read checksum signature: %w", err)
	}
	if len(signature) > maxSignatureBytes {
		return fmt.Errorf("checksum signature exceeds %d bytes", maxSignatureBytes)
	}
	dir, err := os.MkdirTemp("", "pvm-signature-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sumsPath, sigPath := filepath.Join(dir, "checksums"), filepath.Join(dir, "checksums.sig")
	if err := os.WriteFile(sumsPath, sums, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(sigPath, signature, 0600); err != nil {
		return err
	}
	// An empty private home prevents a personal or system-default keyring from
	// expanding trust beyond the explicitly supplied publisher keys.
	verifyCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	cmd := exec.CommandContext(verifyCtx, binary, "--homedir", dir, "--keyring", keyring, sigPath, sumsPath)
	if err := cmd.Run(); err != nil {
		if verifyCtx.Err() != nil {
			return fmt.Errorf("verify checksum signature: %w", verifyCtx.Err())
		}
		return fmt.Errorf("verify checksum signature: %w; check the trusted publisher keyring and checksum signature", err)
	}
	return nil
}
