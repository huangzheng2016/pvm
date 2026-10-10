package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateInputs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"agent-linux-" + runtime.GOARCH,
		"gateway-linux-" + runtime.GOARCH,
		"manifest.json",
		"agent-linux-" + runtime.GOARCH + ".zst",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInputs(entries); err != nil {
		t.Fatalf("valid native inputs rejected: %v", err)
	}
}

func TestValidateInputsRejectsOtherArch(t *testing.T) {
	dir := t.TempDir()
	other := "amd64"
	if runtime.GOARCH == other {
		other = "arm64"
	}
	for _, name := range []string{
		"agent-linux-" + runtime.GOARCH,
		"gateway-linux-" + runtime.GOARCH,
		"agent-linux-" + other,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInputs(entries); err == nil {
		t.Fatal("other-arch payload was accepted")
	}
}

func TestValidateInputsRequiresBoth(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent-linux-"+runtime.GOARCH), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInputs(entries); err == nil {
		t.Fatal("missing gateway was accepted")
	}
}
