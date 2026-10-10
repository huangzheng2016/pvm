package cli

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type kasmView struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	HostPort int    `json:"host_port"`
}

// CmdKasm implements `pvm kasm <vm> [--open]`: ensure the kasm desktop
// container runs in the VM and print its local URL + credentials.
func CmdKasm(args []string) int {
	if len(args) < 1 {
		return fatalf("usage: pvm kasm <vm> [--open]")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var v kasmView
	if err := c.Post("/v1/vms/"+args[0]+"/kasm", nil, &v); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("kasm ready: %s\n  user:     %s\n  password: %s\n", v.URL, v.Username, v.Password)
	if hasFlag(args[1:], "--open") {
		if runtime.GOOS == "darwin" {
			exec.Command("open", v.URL).Start()
		} else {
			exec.Command("xdg-open", v.URL).Start()
		}
	}
	return 0
}
