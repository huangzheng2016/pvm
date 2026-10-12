package gateway

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

// validDISCOVER builds a minimal BOOTREQUEST the parser accepts.
func validDISCOVER(hlen byte) []byte {
	b := make([]byte, 300)
	b[0] = 1 // BOOTREQUEST
	b[1] = 1 // htype ethernet
	b[2] = hlen
	binary.BigEndian.PutUint32(b[4:8], 0xdeadbeef)
	copy(b[28:28+6], []byte{0x52, 0x54, 0x00, 0x11, 0x22, 0x33})
	copy(b[236:240], []byte{99, 130, 83, 99})
	// option 53 = DISCOVER, then end
	i := 240
	b[i], b[i+1], b[i+2] = 53, 1, 1
	b[i+3] = 255
	return b[:i+4]
}

func TestParseDHCPValid(t *testing.T) {
	r, err := parseDHCP(validDISCOVER(6))
	if err != nil {
		t.Fatalf("valid packet: %v", err)
	}
	if r.mac != "52:54:00:11:22:33" || r.msgType != 1 {
		t.Fatalf("parsed %+v", r)
	}
}

// A crafted hlen must not slice out of range or panic the gateway agent.
func TestParseDHCPRejectsBadHlen(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkt  []byte
	}{
		{"hlen beyond packet", validDISCOVER(255)[:250]},
		{"hlen over 16", validDISCOVER(17)},
		{"hlen 250", validDISCOVER(250)},
		{"htype not ethernet", func() []byte { b := validDISCOVER(6); b[1] = 9; return b }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDHCP(tc.pkt); err == nil {
				t.Fatal("bad htype/hlen must be rejected")
			}
		})
	}
}

// Truncations and bit flips of a valid packet must never panic.
func TestParseDHCPNeverPanics(t *testing.T) {
	base := validDISCOVER(6)
	for n := 0; n <= len(base); n++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic at len %d: %v", n, r)
				}
			}()
			_, _ = parseDHCP(base[:n])
		}()
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 500; i++ {
		pkt := append([]byte(nil), base...)
		for j := 0; j < 8; j++ {
			pkt[rng.Intn(len(pkt))] = byte(rng.Intn(256))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on mutated packet: %v", r)
				}
			}()
			_, _ = parseDHCP(pkt)
		}()
	}
}
