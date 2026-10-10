// Package config loads ~/.pvm/config.yaml.
package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Defaults Defaults `yaml:"defaults"`
}

type Defaults struct {
	CPUs   int `yaml:"cpus"`
	MemMB  int `yaml:"mem_mb"`
	DiskGB int `yaml:"disk_gb"`
}

func Default() *Config {
	return &Config{Defaults: Defaults{CPUs: 2, MemMB: 4096, DiskGB: 20}}
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
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Defaults.CPUs <= 0 {
		cfg.Defaults.CPUs = 2
	}
	if cfg.Defaults.MemMB <= 0 {
		cfg.Defaults.MemMB = 4096
	}
	if cfg.Defaults.DiskGB <= 0 {
		cfg.Defaults.DiskGB = 20
	}
	return cfg, nil
}
