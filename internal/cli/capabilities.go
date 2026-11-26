package cli

import (
	"os"
	"runtime"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
)

// CmdCapabilities reports the machine-readable contract offline: it never
// creates directories, spawns the daemon, or mutates configuration.
func CmdCapabilities(args []string) int {
	args = scanJSONFlag(args)
	backend := os.Getenv("PVM_BACKEND")
	if backend == "" {
		backend = "auto"
	}
	caps := map[string]any{
		"protocol": "pvm-agent/1",
		"version":  buildinfo.Version,
		"goos":     runtime.GOOS,
		"goarch":   runtime.GOARCH,
		"backend":  backend,
		"features": map[string]bool{
			"virtio9p": runtime.GOOS != "windows",
			"stream":   true,
			"app":      true,
			"desktop":  true,
			"wsl":      runtime.GOOS == "windows",
			"jobs":     true,
		},
		"json_commands": []string{
			"capabilities", "list", "daemon status", "mount ls", "map ls",
			"route ls", "route explain", "route pause", "route resume",
			"route require-check", "image list", "image default", "workspace ls",
			"workspace status", "exit ls", "exit import", "exit nodes",
			"doctor", "audit", "check", "diag", "dashboard", "ssh-config",
			"default", "job", "job ls", "create", "start",
		},
		"exit_codes": map[string]int{
			"completed":            0,
			"operation_failed":     1,
			"invalid_input":        2,
			"user_action_required": 3,
			"wait_timeout":         4,
		},
	}
	if err := writeJSONOut(caps); err != nil {
		return failErr(err)
	}
	return 0
}
