package cli

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/vm"
)

const mountUsage = "usage: pvm mount <vm> [--profile coherent|readahead] [--read-only] <host-path>:<guest-path> | ls <vm> | rm <vm> <guest-path>"

type mountView struct {
	Tag      string `json:"tag"`
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	Profile  string `json:"profile"`
	ReadOnly bool   `json:"read_only"`
}

type mountAddOptions struct {
	vmName   string
	host     string
	guest    string
	profile  string
	readOnly bool
}

func parseMountAddArgs(args []string) (mountAddOptions, error) {
	if len(args) < 2 {
		return mountAddOptions{}, fmt.Errorf("%s", mountUsage)
	}
	flags := flag.NewFlagSet("mount", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	readOnly := flags.Bool("read-only", false, "enforce read-only access (VM must be stopped)")
	profile := flags.String("profile", "", "coherent or readahead")
	if err := flags.Parse(args[1:]); err != nil {
		return mountAddOptions{}, err
	}
	if len(flags.Args()) != 1 {
		return mountAddOptions{}, fmt.Errorf("%s", mountUsage)
	}
	host, guest, ok := splitMountSpec(flags.Args()[0])
	if !ok || host == "" || guest == "" {
		return mountAddOptions{}, fmt.Errorf("invalid mount %q, want <host-path>:<guest-path>", flags.Args()[0])
	}
	normalized, err := vm.NormalizeMountProfile(*profile)
	if err != nil {
		return mountAddOptions{}, err
	}
	return mountAddOptions{vmName: args[0], host: host, guest: guest, profile: normalized, readOnly: *readOnly}, nil
}

// splitMountSpec splits "<host-path>:<guest-path>" while preserving a leading
// Windows drive letter such as "C:\work:/home/dev/work" or "C:/work:/mnt/work".
func splitMountSpec(spec string) (string, string, bool) {
	if hasWindowsDrivePrefix(spec) {
		rest := spec[2:]
		idx := strings.IndexByte(rest, ':')
		if idx < 0 {
			return "", "", false
		}
		return spec[:2+idx], rest[idx+1:], true
	}
	host, guest, ok := strings.Cut(spec, ":")
	if !ok {
		return "", "", false
	}
	return host, guest, true
}

func hasWindowsDrivePrefix(s string) bool {
	if len(s) < 3 || s[1] != ':' {
		return false
	}
	c := s[0]
	isDrive := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	return isDrive && (s[2] == '\\' || s[2] == '/')
}

func writeMountTable(w io.Writer, mounts []mountView) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tGUEST\tPROFILE\tACCESS")
	for _, m := range mounts {
		profile, err := vm.NormalizeMountProfile(m.Profile)
		if err != nil {
			return err
		}
		access := "rw"
		if m.ReadOnly {
			access = "ro"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Host, m.Guest, profile, access)
	}
	return tw.Flush()
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
		if err := writeMountTable(os.Stdout, mounts); err != nil {
			return fatalf("%v", err)
		}
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
	if len(args) >= 1 && args[0] != "ls" && args[0] != "rm" {
		add, err := parseMountAddArgs(args)
		if err != nil {
			return fatalf("%v", err)
		}
		host := add.host
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
		host, err = filepath.Abs(host)
		if err != nil {
			return fatalf("mount host path: %v", err)
		}

		c, err := daemon.Ensure(p)
		if err != nil {
			return fatalf("%v", err)
		}
		var m mountView
		body := map[string]any{"host": host, "guest": add.guest, "profile": add.profile, "read_only": add.readOnly}
		if err := c.Post("/v1/vms/"+add.vmName+"/mounts", body, &m); err != nil {
			return fatalf("mount: %v", err)
		}
		fmt.Printf("mounted %s -> %s:%s\n", m.Host, add.vmName, m.Guest)
		return 0
	}
	return fatalf("%s", mountUsage)
}
