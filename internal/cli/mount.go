package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type mountView struct {
	Tag   string `json:"tag"`
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

func CmdMount(args []string) int {
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	if len(args) == 2 && args[0] == "ls" {
		c, err := daemon.Ensure(p)
		if err != nil {
			return fatalf("%v", err)
		}
		var mounts []mountView
		if err := c.Get("/v1/vms/"+args[1]+"/mounts", &mounts); err != nil {
			return fatalf("%v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tGUEST")
		for _, m := range mounts {
			fmt.Fprintf(tw, "%s\t%s\n", m.Host, m.Guest)
		}
		tw.Flush()
		return 0
	}
	if len(args) == 3 && args[0] == "rm" {
		c, err := daemon.Ensure(p)
		if err != nil {
			return fatalf("%v", err)
		}
		path := "/v1/vms/" + args[1] + "/mounts?guest=" + url.QueryEscape(args[2])
		if err := c.Delete(path, nil); err != nil {
			return fatalf("mount rm: %v", err)
		}
		fmt.Printf("unmounted %s:%s\n", args[1], args[2])
		return 0
	}
	if len(args) == 2 && args[0] != "ls" && args[0] != "rm" {
		host, guest, ok := strings.Cut(args[1], ":")
		if !ok || host == "" || guest == "" {
			return fatalf("invalid mount %q, want <host-path>:<guest-path>", args[1])
		}
		if host == "~" || strings.HasPrefix(host, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return fatalf("%v", err)
			}
			if host == "~" {
				host = home
			} else {
				host = filepath.Join(home, strings.TrimPrefix(host, "~/"))
			}
		}
		c, err := daemon.Ensure(p)
		if err != nil {
			return fatalf("%v", err)
		}
		var m mountView
		body := map[string]string{"host": host, "guest": guest}
		if err := c.Post("/v1/vms/"+args[0]+"/mounts", body, &m); err != nil {
			return fatalf("mount: %v", err)
		}
		fmt.Printf("mounted %s -> %s:%s\n", m.Host, args[0], m.Guest)
		return 0
	}
	return fatalf("usage: pvm mount <vm> <host-path>:<guest-path> | ls <vm> | rm <vm> <guest-path>")
}
