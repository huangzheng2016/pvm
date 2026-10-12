package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"

	"github.com/huangzheng2016/pvm/internal/qemu"
)

type resizeRequest struct {
	SizeGB int `json:"size_gb"`
}

// handleResize grows a VM's qcow2 (grow-only; shrinking is refused).
// The VM must be stopped: qemu-img cannot touch a live disk, and the
// guest's cloud-init growpart/resizefs modules expand the partition and
// filesystem to the new ceiling automatically on the next boot.
func (s *Server) handleResize(w http.ResponseWriter, r *http.Request) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	var req resizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.SizeGB <= spec.DiskGB {
		writeErr(w, 400, fmt.Errorf("size %dG must be larger than the current %dG; shrinking is not supported", req.SizeGB, spec.DiskGB))
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
	disk := qemu.ActiveDisk(vmDir, spec.Disk(s.P))
	out, err := exec.Command(s.TC.Img, "resize", disk, fmt.Sprintf("%dG", req.SizeGB)).CombinedOutput()
	if err != nil {
		writeErr(w, 500, fmt.Errorf("qemu-img resize: %v\n%s", err, out))
		return
	}
	spec.DiskGB = req.SizeGB
	if err := spec.Save(s.P); err != nil {
		writeErr(w, 500, fmt.Errorf("disk resized but spec save failed: %v", err))
		return
	}
	writeJSON(w, 200, map[string]any{
		"vm":      spec.Name,
		"disk_gb": req.SizeGB,
		"note":    "the guest grows its partition and filesystem to the new size automatically on the next boot (cloud-init)",
	})
}
