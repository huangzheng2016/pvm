package assets

import (
	"github.com/huangzheng2016/pvm/internal/native"
	"strings"
	"testing"
)

func TestManifestRequiresCompleteNativeAssets(t *testing.T) {
	valid := []manifestEntry{{Path: native.GuestBinary("agent"), SHA256: strings.Repeat("a", 64), Mode: 0o755}, {Path: native.GuestBinary("gateway"), SHA256: strings.Repeat("b", 64), Mode: 0o755}}
	if err := validateManifest(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]manifestEntry{"empty": nil, "missing gateway": valid[:1], "duplicate": {valid[0], valid[0]}, "unexpected": {{Path: "other", SHA256: valid[0].SHA256, Mode: 0o755}, valid[1]}, "bad hash": {{Path: valid[0].Path, SHA256: "bad", Mode: 0o755}, valid[1]}, "unsafe mode": {{Path: valid[0].Path, SHA256: valid[0].SHA256, Mode: 0o777}, valid[1]}}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			if validateManifest(entries) == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
