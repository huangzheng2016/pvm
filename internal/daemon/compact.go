package daemon

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/huangzheng2016/pvm/internal/qemu"
)

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	if st, err := s.instance(spec).Running(); err != nil {
		writeErr(w, 500, err)
		return
	} else if st != nil {
		writeErr(w, 409, fmt.Errorf("vm %s is running; stop it first (`pvm stop %s`)", spec.Name, spec.Name))
		return
	}
	vmDir := s.P.VMDir(spec.Name)
	manifest, err := qemu.LoadManifest(vmDir)
	if err != nil && !errors.Is(err, qemu.ErrNoSnapshot) {
		writeErr(w, 500, err)
		return
	}
	if manifest != nil && !manifest.Consumed {
		writeErr(w, 409, fmt.Errorf("vm %s has an unconsumed RAM snapshot; restore it with `pvm start %s` before compacting", spec.Name, spec.Name))
		return
	}
	disk := qemu.ActiveDisk(vmDir, spec.Disk(s.P))
	result, err := qemu.CompactDisk(s.TC, disk)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	reclaimed := int64(0)
	if result.Changed {
		reclaimed = result.BeforeBytes - result.AfterBytes
	}
	writeJSON(w, 200, map[string]any{
		"vm":              spec.Name,
		"before_bytes":    result.BeforeBytes,
		"after_bytes":     result.AfterBytes,
		"reclaimed_bytes": reclaimed,
		"changed":         result.Changed,
	})
}
