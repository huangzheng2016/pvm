package cli

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

func CmdDashboard(args []string) int {
	args = scanJSONFlag(args)
	openBrowser := false
	for _, a := range args {
		if a == "--open" || a == "-open" {
			openBrowser = true
		} else {
			return failErr(fmt.Errorf("usage: pvm dashboard [--open] [--json]"))
		}
	}
	p, err := mustPaths()
	if err != nil {
		return failErr(err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return failErr(err)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.Post("/v1/dashboard", nil, &out); err != nil {
		return failErr(err)
	}
	if jsonOutput {
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": out}))
	}
	fmt.Printf("dashboard (read-only): %s\n", out.URL)
	fmt.Fprintln(os.Stderr, "note: the URL carries a local read-only token; do not share it")
	if openBrowser {
		switch runtime.GOOS {
		case "darwin":
			_ = exec.Command("open", out.URL).Start()
		case "windows":
			_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", out.URL).Start()
		default:
			_ = exec.Command("xdg-open", out.URL).Start()
		}
	}
	return 0
}
