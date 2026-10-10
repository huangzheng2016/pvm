package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// GenerateKey returns a base64 WireGuard private key.
func GenerateKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[0] &= 248
	b[31] = (b[31] & 127) | 64
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// PublicKey derives the base64 public key for a base64 private key.
func PublicKey(privB64 string) (string, error) {
	priv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil || len(priv) != 32 {
		return "", fmt.Errorf("invalid private key")
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// Hex encodes a base64 key as hex for IpcSet.
func Hex(keyB64 string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("invalid wireguard key")
	}
	return hex.EncodeToString(b), nil
}
