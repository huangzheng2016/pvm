package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

func replaceBinary(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pvm-replacing-*")
	if err != nil {
		return err
	}
	previous := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(previous)
		return err
	}
	if err := os.Remove(previous); err != nil {
		return err
	}
	if err := os.Rename(path, previous); err != nil {
		return fmt.Errorf("move running executable aside: %w", err)
	}
	if err := fsutil.AtomicWrite(path, data, 0o755); err != nil {
		return errors.Join(fmt.Errorf("install replacement executable: %w", err), os.Rename(previous, path))
	}
	if err := os.Remove(previous); err != nil {
		fmt.Printf("previous executable retained at %s (remove it after this command exits): %v\n", previous, err)
	}
	return nil
}
