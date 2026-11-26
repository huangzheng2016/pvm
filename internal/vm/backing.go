package vm

import (
	"path/filepath"
	"strings"

	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

// RelativeBacking stores a qcow2 backing path relative to the overlay when
// both live under PVM_HOME, so moving PVM_HOME to another disk keeps every
// VM bootable without rewriting disk metadata.
func RelativeBacking(p *paths.Paths, overlay, baseDisk string) string {
	backing := qemu.BackingPath(baseDisk)
	absBase, err := filepath.Abs(backing)
	if err != nil {
		return backing
	}
	homeAbs, err := filepath.Abs(p.Home)
	if err != nil {
		return backing
	}
	if !strings.HasPrefix(absBase, homeAbs+string(filepath.Separator)) {
		return backing
	}
	if rel, err := filepath.Rel(filepath.Dir(overlay), absBase); err == nil && !filepath.IsAbs(rel) {
		return filepath.ToSlash(rel)
	}
	return backing
}
