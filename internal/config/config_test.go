package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/native"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	for _, tt := range []struct {
		name, data          string
		wantMem             int
		wantUpdate, wantErr bool
	}{
		{"empty", "", native.DefaultMemMB(), false, false},
		{"zero", "defaults:\n  mem_mb: 0\n", native.DefaultMemMB(), false, false},
		{"negative", "defaults:\n  mem_mb: -1\n", native.DefaultMemMB(), false, false},
		{"explicit", "defaults:\n  mem_mb: 2048\nupdate_check: true\n", 2048, true, false},
		{"unknown", "defaults:\n  mem_mbb: 2048\n", 0, false, true},
		{"documents", "defaults: {}\n---\nupdate_check: true\n", 0, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err == nil && (cfg.Defaults.MemMB != tt.wantMem || cfg.UpdateCheck != tt.wantUpdate) {
				t.Fatalf("config = %+v", cfg)
			}
		})
	}
	if cfg, err := Load(filepath.Join(t.TempDir(), "absent")); err != nil || cfg.UpdateCheck {
		t.Fatalf("missing config: %v, %v", cfg, err)
	}
}
