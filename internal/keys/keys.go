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

	"golang.org/x/crypto/ssh"
)

// Ensure creates ~/.pvm/keys/id_ed25519{,.pub} if missing and returns the
// authorized_keys-formatted public key.
func Ensure(keyPath string) (pubAuthorized string, err error) {
	if pub, err := os.ReadFile(keyPath + ".pub"); err == nil {
		return string(pub), nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	privPEM, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(privPEM), 0o600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(sshPub))
	if err := os.WriteFile(keyPath+".pub", []byte(pubLine), 0o644); err != nil {
		return "", fmt.Errorf("write public key: %w", err)
	}
	return pubLine, nil
}
