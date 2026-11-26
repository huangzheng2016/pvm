package qemu

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

// InspectBacking returns the resolved full backing filename of a qcow2 overlay.
func InspectBacking(img, overlay string) (string, error) {
	cmd := exec.Command(img, "info", "--output=json", overlay)
	fsutil.ConfigureHiddenCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var info struct {
		FullBacking string `json:"full-backing-filename"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", err
	}
	return info.FullBacking, nil
}

// RebaseBacking rewrites only the backing pointer (-u) of a stopped overlay;
// the target backing image must hold identical content to the old one.
func RebaseBacking(img, overlay, newBacking string) error {
	cmd := exec.Command(img, "rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", newBacking, overlay)
	fsutil.ConfigureHiddenCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img rebase: %v\n%s", err, out)
	}
	return nil
}
