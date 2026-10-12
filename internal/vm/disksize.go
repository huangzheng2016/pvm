package vm

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/huangzheng2016/pvm/internal/qemu"
)

func virtualSizeGB(tc *qemu.Toolchain, path string) (int, error) {
	out, err := exec.Command(tc.Img, "info", "--output=json", path).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("qemu-img info: %v\n%s", err, out)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("qemu-img info parse: %w", err)
	}
	gb := int64(1) << 30
	return int((info.VirtualSize + gb - 1) / gb), nil
}
