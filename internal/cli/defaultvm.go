package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
	"github.com/huangzheng2016/pvm/internal/workspace"
)

// resolveVM fills in the VM name a command was given without. The cwd's
// workspace wins: the folder a person stands in already says which VM
// they mean, and one dir can only be inside one checkout at a time.
// default_vm from config.yaml follows, then the only user VM.
//
// It returns ws and rel when the cwd lies inside a workspace, so callers
// that land the user in a shell (`pvm ssh`) can start there.
func resolveVM(c *daemon.Client, p *paths.Paths, explicit string) (name string, ws *workspace.Workspace, rel string, err error) {
	if explicit != "" {
		if ws, rel = cwdWorkspace(p); ws != nil {
			if ws.VM != explicit {
				ws, rel = nil, ""
			}
		}
		return explicit, ws, rel, nil
	}
	if ws, rel = cwdWorkspace(p); ws != nil {
		if ws.VM != "" {
			if err := checkVMMatch(c, ws); err != nil {
				return "", nil, "", err
			}
			fmt.Fprintf(os.Stderr, "pvm: using vm %s (workspace %s)\n", ws.VM, ws.ID)
			return ws.VM, ws, rel, nil
		}
	}
	var infos []vmInfoView
	if err := c.Get("/v1/vms", &infos); err != nil {
		return "", nil, "", err
	}
	var names []string
	for _, in := range infos {
		if in.Spec.Role != "gateway" {
			names = append(names, in.Spec.Name)
		}
	}
	sort.Strings(names)
	cfg, err := config.Load(p.ConfigFile())
	if err != nil {
		return "", nil, "", err
	}
	if cfg.DefaultVM != "" {
		// Falling back to another VM would run the command on a machine
		// the user did not choose.
		if !slices.Contains(names, cfg.DefaultVM) {
			return "", nil, "", fmt.Errorf("default vm %s does not exist; choose another with `pvm default-vm <vm>` or clear it with `pvm default-vm --clear`", cfg.DefaultVM)
		}
		fmt.Fprintf(os.Stderr, "pvm: using the default vm, %s\n", cfg.DefaultVM)
		return cfg.DefaultVM, ws, rel, nil
	}
	switch len(names) {
	case 1:
		fmt.Fprintf(os.Stderr, "pvm: using the only vm, %s\n", names[0])
		return names[0], ws, rel, nil
	case 0:
		return "", nil, "", fmt.Errorf("no vm yet; create one with `pvm create dev1`")
	}
	hint := "several vms and no workspace here; name one: " + strings.Join(names, ", ")
	if ws != nil {
		hint = fmt.Sprintf("workspace %s has no vm recorded; name one: %s", ws.ID, strings.Join(names, ", "))
	}
	return "", nil, "", fmt.Errorf("%s", hint)
}

// cwdWorkspace returns the innermost workspace containing the current
// directory and the path relative to it, slash-separated.
func cwdWorkspace(p *paths.Paths) (*workspace.Workspace, string) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, ""
	}
	all, err := workspace.NewStore(p).List()
	if err != nil {
		return nil, ""
	}
	var best *workspace.Workspace
	bestRel := ""
	bestLen := -1
	for _, ws := range all {
		rel, ok := relInside(ws.HostPath, cwd)
		if !ok {
			continue
		}
		if len(ws.HostPath) > bestLen {
			best, bestRel, bestLen = ws, rel, len(ws.HostPath)
		}
	}
	return best, bestRel
}

// relInside reports whether p is root or below it and returns the
// relative path using p's own spelling, so a case-insensitive match on
// Windows keeps the directory names the user typed. The root itself is
// "".
func relInside(root, p string) (string, bool) {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if root == p {
		return "", true
	}
	if p == string(filepath.Separator) || len(p) <= len(root) {
		return "", false
	}
	if !strings.EqualFold(p[:len(root)], root) {
		return "", false
	}
	sep := p[len(root)]
	if sep != filepath.Separator {
		return "", false
	}
	return filepath.ToSlash(p[len(root)+1:]), true
}

// checkVMMatch reports a workspace whose VM no longer exists instead of
// quietly using another VM: the folder says which machine holds that
// tree, and guessing would point the command at the wrong one.
func checkVMMatch(c *daemon.Client, ws *workspace.Workspace) error {
	var info vmInfoView
	if err := c.Get("/v1/vms/"+ws.VM, &info); err != nil {
		return fmt.Errorf("workspace %s belongs to vm %s, which the daemon does not know: %v\nrecreate it or remove the workspace (`pvm workspace rm %s -y`)", ws.ID, ws.VM, err, ws.ID)
	}
	return nil
}

// guestDir is the directory a command should start in: the workspace's
// guest path plus the cwd's offset. ok is false outside a workspace.
func guestDir(ws *workspace.Workspace, rel string) (string, bool) {
	if ws == nil || ws.GuestPath == "" {
		return "", false
	}
	if rel == "" {
		return ws.GuestPath, true
	}
	return ws.GuestPath + "/" + rel, true
}

// CmdDefaultVM prints, sets or clears default_vm in config.yaml.
func CmdDefaultVM(args []string) int {
	fs := newFlagSet("default-vm", "[<vm>|--clear]")
	clear := fs.Bool("clear", false, "remove the default vm")
	if err := fs.Parse(partitionFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 || (*clear && fs.NArg() > 0) {
		fs.Usage()
		return 2
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	switch {
	case *clear:
		if err := config.SetDefaultVM(p.ConfigFile(), ""); err != nil {
			return fatalf("%v", err)
		}
		fmt.Println("default vm cleared")
	case fs.NArg() == 0:
		cfg, err := config.Load(p.ConfigFile())
		if err != nil {
			return fatalf("%v", err)
		}
		if cfg.DefaultVM == "" {
			fmt.Println("no default vm; set one with `pvm default-vm <vm>`")
			return 0
		}
		fmt.Println(cfg.DefaultVM)
	default:
		name := fs.Arg(0)
		if err := vm.ValidateName(name); err != nil {
			return fatalf("%v", err)
		}
		c, err := ensureDaemon(p)
		if err != nil {
			return fatalf("%v", err)
		}
		var info vmInfoView
		if err := c.Get("/v1/vms/"+name, &info); err != nil {
			return fatalf("%v", err)
		}
		if info.Spec.Role == "gateway" {
			return fatalf("%s is a gateway vm and cannot be the default vm", name)
		}
		if err := config.SetDefaultVM(p.ConfigFile(), name); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("default vm: %s\n", name)
	}
	return 0
}
