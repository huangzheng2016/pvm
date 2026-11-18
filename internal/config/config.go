package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type Config struct {
	Defaults    Defaults `yaml:"defaults"`
	UpdateCheck bool     `yaml:"update_check"`
	Backend     string   `yaml:"backend,omitempty"` // auto | native | wsl (Windows host mode)
	DefaultVM   string   `yaml:"default_vm,omitempty"`
}

type Defaults struct {
	CPUs   int `yaml:"cpus"`
	MemMB  int `yaml:"mem_mb"`
	DiskGB int `yaml:"disk_gb"`
}

func Default() *Config {
	return &Config{Defaults: Defaults{CPUs: native.DefaultCPUs(), MemMB: native.DefaultMemMB(), DiskGB: 20}}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	return parse(data)
}

func parse(data []byte) (*Config, error) {
	cfg := Default()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil && err != io.EOF {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("config must contain exactly one YAML document")
	}
	if cfg.Defaults.CPUs <= 0 {
		cfg.Defaults.CPUs = native.DefaultCPUs()
	}
	if cfg.Defaults.MemMB <= 0 {
		cfg.Defaults.MemMB = native.DefaultMemMB()
	}
	if cfg.Defaults.DiskGB <= 0 {
		cfg.Defaults.DiskGB = 20
	}
	switch cfg.Backend {
	case "", "auto", "native", "wsl":
	default:
		return nil, fmt.Errorf("invalid backend %q (want auto, native, or wsl)", cfg.Backend)
	}
	if cfg.DefaultVM != "" {
		if err := vm.ValidateName(cfg.DefaultVM); err != nil {
			return nil, fmt.Errorf("default_vm: %w", err)
		}
	}
	return cfg, nil
}

// SetDefaultVM stores name as default_vm, or removes the key when name is
// empty. It edits that one line of the file's text instead of writing
// the loaded Config back: Load fills the host-derived defaults, which
// must not end up pinned in the file, and the user's comments and other
// keys stay as they wrote them. A file Load rejects is left untouched.
func SetDefaultVM(path, name string) error {
	if name != "" {
		if err := vm.ValidateName(name); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	want, err := parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if want.DefaultVM == name {
		return nil
	}
	want.DefaultVM = name

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	lines := strings.SplitAfter(string(data), "\n")
	var key, val *yaml.Node
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		pairs := doc.Content[0].Content
		for i := 0; i+1 < len(pairs); i += 2 {
			if pairs[i].Value == "default_vm" {
				key, val = pairs[i], pairs[i+1]
			}
		}
	}
	// a name such as null is not a string unless quoted
	scalar := name
	var plain string
	if yaml.Unmarshal([]byte(name), &plain) != nil || plain != name {
		scalar = `"` + name + `"`
	}
	var out string
	switch {
	// YAML also ends a line at a bare CR, which the split above does not
	case key != nil && val.Line > len(lines):
		return fmt.Errorf("%s: cannot edit default_vm in place; set it by hand", path)
	case key != nil:
		// a plain value may sit on the line after its key
		repl := ""
		if name != "" {
			repl = "default_vm: " + scalar
			if val.LineComment != "" {
				repl += " " + val.LineComment
			}
			repl += eol
		}
		out = strings.Join(lines[:key.Line-1], "") + repl + strings.Join(lines[val.Line:], "")
	default:
		out = string(data)
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += eol
		}
		out += "default_vm: " + scalar + eol
	}
	// The edit is textual, so prove it changed nothing else before the
	// file is replaced (a flow-style or otherwise unusual layout).
	got, err := parse([]byte(out))
	if err != nil || *got != *want {
		return fmt.Errorf("%s: cannot edit default_vm in place; set it by hand", path)
	}
	return fsutil.AtomicWrite(path, []byte(out), 0o600)
}
