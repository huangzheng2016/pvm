package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"

	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type resizeRequest struct {
	SizeGB int `json:"size_gb"`
}

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
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	spec, err = vm.Load(s.P, spec.Name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if req.SizeGB < 1 || req.SizeGB > 4096 {
		writeErr(w, 400, fmt.Errorf("size must be between 1 and 4096 GiB"))
		return
	}
	if req.SizeGB <= spec.DiskGB {
		writeErr(w, 400, fmt.Errorf("size %dG must be larger than the current %dG; shrinking is not supported", req.SizeGB, spec.DiskGB))
		return
	}
	if st, err := s.instance(spec).Running(); err != nil {
		writeErr(w, 500, err)
		return
	} else if st != nil {
		writeErr(w, 409, fmt.Errorf("vm %s is running; stop it first (`pvm stop %s`)", spec.Name, spec.Name))
		return
	}
	vmDir := s.P.VMDir(spec.Name)
	disk := qemu.ActiveDisk(vmDir, spec.Disk(s.P))
	out, err := exec.Command(s.TC.Img, "info", "--output=json", disk).CombinedOutput()
	if err != nil {
		writeErr(w, 500, fmt.Errorf("inspect active disk: %v\n%s", err, out))
		return
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.VirtualSize <= 0 {
		writeErr(w, 500, fmt.Errorf("invalid active disk size response"))
		return
	}
	requestedBytes := int64(req.SizeGB) << 30
	if requestedBytes < info.VirtualSize {
		writeErr(w, 400, fmt.Errorf("active disk is already larger than %dG; shrinking is not supported", req.SizeGB))
		return
	}
	if requestedBytes > info.VirtualSize {
		out, err = exec.Command(s.TC.Img, "resize", disk, fmt.Sprintf("%dG", req.SizeGB)).CombinedOutput()
		if err != nil {
			writeErr(w, 500, fmt.Errorf("qemu-img resize: %v\n%s", err, out))
			return
		}
	}
	spec.DiskGB = req.SizeGB
	if err := spec.Save(s.P); err != nil {
		writeErr(w, 500, fmt.Errorf("disk resized but spec save failed: %v; fix state storage and retry `pvm resize %s %d` before starting the VM", err, spec.Name, req.SizeGB))
		return
	}
	s.cacheSpec(spec)
	writeJSON(w, 200, map[string]any{
		"vm":      spec.Name,
		"disk_gb": req.SizeGB,
		"note":    "the guest grows its partition and filesystem to the new size automatically on the next boot (cloud-init)",
	})
}
