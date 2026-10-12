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
}

func CmdDesktop(args []string) int {
	if len(args) < 1 {
		return fatalf("usage: pvm desktop <vm> [--size WxH] [--open] [--stop]")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	if hasFlag(args[1:], "--stop") {
		var v map[string]any
		if err := c.Delete("/v1/vms/"+args[0]+"/desktop", &v); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("desktop stopped: %s\n", args[0])
		return 0
	}
	q := url.Values{}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--size=") {
			q.Set("size", strings.TrimPrefix(a, "--size="))
		}
	}
	var v desktopView
	if err := c.Post("/v1/vms/"+args[0]+"/desktop?"+q.Encode(), nil, &v); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("desktop ready: %s\n", v.URL)
	if hasFlag(args[1:], "--open") {
		if runtime.GOOS == "darwin" {
			exec.Command("open", v.URL).Start()
		} else {
			exec.Command("xdg-open", v.URL).Start()
		}
	}
	return 0
}
