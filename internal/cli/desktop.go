package cli

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type desktopView struct {
	URL      string `json:"url"`
	HostPort int    `json:"host_port"`
	Public   bool   `json:"public"`
}

func CmdDesktop(args []string) int {
	fs := newFlagSet("desktop", "<vm> [--size WxH] [--open] [--public] [--stop]")
	size := fs.String("size", "1920x1080", "desktop resolution")
	openBrowser := fs.Bool("open", false, "open the desktop URL in a browser")
	public := fs.Bool("public", false, "listen on 0.0.0.0 (expose noVNC to the network)")
	stop := fs.Bool("stop", false, "stop the desktop session")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if name == "" {
		if fs.NArg() != 1 {
			fs.Usage()
			return 2
		}
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	if *stop {
		var v map[string]any
		if err := c.Delete("/v1/vms/"+name+"/desktop", &v); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("desktop stopped: %s\n", name)
		return 0
	}
	q := url.Values{}
	q.Set("size", *size)
	if *public {
		q.Set("public", "1")
	}
	var v desktopView
	if err := c.Post("/v1/vms/"+name+"/desktop?"+q.Encode(), nil, &v); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("desktop ready: %s\n", v.URL)
	if v.Public {
		fmt.Println("WARNING: noVNC is listening on 0.0.0.0; anyone who can reach this host can attempt the VNC password")
	}
	if *openBrowser {
		if runtime.GOOS == "darwin" {
			exec.Command("open", v.URL).Start()
		} else {
			exec.Command("xdg-open", v.URL).Start()
		}
	}
	return 0
}
