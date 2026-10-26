// Package keys manages the pvm-dedicated SSH keypair. It never touches
// ~/.ssh so the VM fleet stays fingerprint-isolated from the user's
// existing identity.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"golang.org/x/crypto/ssh"
)

func Ensure(keyPath string) (pubAuthorized string, err error) {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return "", err
	}
	lock, err := fsutil.TryLock(keyPath + ".lock")
	if err != nil {
		return "", fmt.Errorf("ssh key is in use; retry: %w", err)
	}
	defer lock.Unlock()
	privPEM, privErr := os.ReadFile(keyPath)
	pub, pubErr := os.ReadFile(keyPath + ".pub")
	if privErr != nil && !os.IsNotExist(privErr) {
		return "", privErr
	}
	if pubErr != nil && !os.IsNotExist(pubErr) {
		return "", pubErr
	}
	if privErr == nil {
		signer, err := ssh.ParsePrivateKey(privPEM)
		if err != nil {
			return "", fmt.Errorf("parse private key: %w", err)
		}
		pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
		if pubErr == nil {
			if strings.TrimSpace(string(pub)) != strings.TrimSpace(pubLine) {
				return "", fmt.Errorf("ssh public key does not match private key")
			}
			return pubLine, nil
		}
		if err := fsutil.AtomicWrite(keyPath+".pub", []byte(pubLine), 0o644); err != nil {
			return "", fmt.Errorf("write public key: %w", err)
		}
		return pubLine, nil
	}

	if pubErr == nil {
		return "", fmt.Errorf("ssh private key is missing but public key exists; restore the private key instead of replacing the identity")
	}

	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", err
	}
	generatedPEM, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", err
	}
	if err := fsutil.AtomicWrite(keyPath, pem.EncodeToMemory(generatedPEM), 0o600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(sshPub))
	if err := fsutil.AtomicWrite(keyPath+".pub", []byte(pubLine), 0o644); err != nil {
		return "", fmt.Errorf("write public key: %w", err)
	}
	return pubLine, nil
}
