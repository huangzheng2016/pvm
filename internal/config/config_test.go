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
		{"default vm", "default_vm: dev1\n", native.DefaultMemMB(), false, false},
		{"bad default vm", "default_vm: a/b\n", 0, false, true},
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

func TestSetDefaultVM(t *testing.T) {
	const commented = "# my settings\ndefaults:\n  mem_mb: 2048 # plenty\n\nupdate_check: true\n"
	for _, tt := range []struct {
		name, data, vm, want string
	}{
		{"add keeps comments and keys", commented, "dev1", commented + "default_vm: dev1\n"},
		{"replace", "default_vm: dev1 # mine\nbackend: wsl\n", "dev2", "default_vm: dev2 # mine\nbackend: wsl\n"},
		{"replace value on next line", "default_vm:\n  dev1\nbackend: wsl\n", "dev2", "default_vm: dev2\nbackend: wsl\n"},
		{"clear", "# top\ndefault_vm: dev1\nbackend: wsl\n", "", "# top\nbackend: wsl\n"},
		{"clear when unset", commented, "", commented},
		{"no trailing newline", "backend: wsl", "dev1", "backend: wsl\ndefault_vm: dev1\n"},
		{"crlf", "backend: wsl\r\ndefault_vm: dev1\r\n", "dev2", "backend: wsl\r\ndefault_vm: dev2\r\n"},
		{"empty file", "", "dev1", "default_vm: dev1\n"},
		{"add a name yaml reads as null", "backend: wsl\n", "null", "backend: wsl\ndefault_vm: \"null\"\n"},
		{"replace with a name yaml reads as null", "default_vm: dev1 # mine\n", "Null", "default_vm: \"Null\" # mine\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := SetDefaultVM(path, tt.vm); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tt.want {
				t.Fatalf("file = %q, %v; want %q", got, err, tt.want)
			}
			if cfg, err := Load(path); err != nil || cfg.DefaultVM != tt.vm {
				t.Fatalf("load = %+v, %v", cfg, err)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SetDefaultVM(path, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("clearing created the file: %v", err)
		}
		if err := SetDefaultVM(path, "dev1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != "default_vm: dev1\n" {
			t.Fatalf("file = %q", got)
		}
	})
	for _, tt := range []struct{ name, data, vm string }{
		{"unknown key", "defaults:\n  mem_mbb: 2048\n", "dev1"},
		{"flow mapping", "{backend: wsl, default_vm: dev1}\n", "dev2"},
		{"invalid name", "backend: wsl\n", "a/b"},
		{"bare cr line breaks", "backend: auto\rdefault_vm: dev0\r", "dev1"},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := SetDefaultVM(path, tt.vm); err == nil {
				t.Fatal("no error")
			}
			if got, _ := os.ReadFile(path); string(got) != tt.data {
				t.Fatalf("file rewritten: %q", got)
			}
		})
	}
}
