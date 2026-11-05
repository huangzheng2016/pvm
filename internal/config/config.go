package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/huangzheng2016/pvm/internal/native"
)

type Config struct {
	Defaults    Defaults `yaml:"defaults"`
	UpdateCheck bool     `yaml:"update_check"`
}

type Defaults struct {
	CPUs   int `yaml:"cpus"`
	MemMB  int `yaml:"mem_mb"`
	DiskGB int `yaml:"disk_gb"`
}

func Default() *Config {
	return &Config{Defaults: Defaults{CPUs: native.HostCPUs(), MemMB: native.DefaultMemMB(), DiskGB: 20}}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
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
		cfg.Defaults.CPUs = native.HostCPUs()
	}
	if cfg.Defaults.MemMB <= 0 {
		cfg.Defaults.MemMB = native.DefaultMemMB()
	}
	if cfg.Defaults.DiskGB <= 0 {
		cfg.Defaults.DiskGB = 20
	}
	return cfg, nil
}
