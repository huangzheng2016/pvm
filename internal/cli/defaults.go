package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type defaultsState struct {
	DefaultImage string `json:"default_image,omitempty"`
	DefaultVM    string `json:"default_vm,omitempty"`
}

func loadDefaults(p *paths.Paths) defaultsState {
	var st defaultsState
	raw, err := os.ReadFile(filepath.Join(p.Home, "defaults.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func saveDefaults(p *paths.Paths, st defaultsState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(filepath.Join(p.Home, "defaults.json"), data, 0o600)
}

// CmdDefault shows or sets the host's default VM: `pvm default [<vm>|--clear]`.
func CmdDefault(args []string) int {
	args = scanJSONFlag(args)
	p, err := mustPaths()
	if err != nil {
		return failErr(err)
	}
	st := loadDefaults(p)
	if len(args) == 0 {
		if jsonOutput {
			return failErr(writeJSONOut(map[string]any{"status": "completed", "data": st}))
		}
		defVM := st.DefaultVM
		if defVM == "" {
			defVM = "(auto: single user VM)"
		}
		defImg := st.DefaultImage
		if defImg == "" {
			defImg = "(auto: single golden distro:version)"
		}
		fmt.Printf("default_vm: %s\ndefault_image: %s\n", defVM, defImg)
		return 0
	}
	if len(args) == 1 && (args[0] == "--clear" || args[0] == "-clear") {
		st.DefaultVM = ""
	} else if len(args) == 1 {
		if _, err := vm.Load(p, args[0]); err != nil {
			return failErr(fmt.Errorf("cannot set default vm %q: %w", args[0], err))
		}
		st.DefaultVM = args[0]
	} else {
		return failErr(fmt.Errorf("usage: pvm default [<vm>|--clear]"))
	}
	if err := saveDefaults(p, st); err != nil {
		return failErr(err)
	}
	if jsonOutput {
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": st}))
	}
	if st.DefaultVM == "" {
		fmt.Println("cleared default vm")
	} else {
		fmt.Printf("default vm set to %s\n", st.DefaultVM)
	}
	return 0
}

func cmdImageDefault(p *paths.Paths, args []string) int {
	args = scanJSONFlag(args)
	st := loadDefaults(p)
	if len(args) == 0 {
		if jsonOutput {
			return failErr(writeJSONOut(map[string]any{"status": "completed", "data": map[string]string{"default_image": st.DefaultImage}}))
		}
		if st.DefaultImage == "" {
			fmt.Println("no default image set")
		} else {
			fmt.Println(st.DefaultImage)
		}
		return 0
	}
	if len(args) == 1 && (args[0] == "--clear" || args[0] == "-clear") {
		st.DefaultImage = ""
	} else if len(args) == 1 {
		if _, err := image.Resolve(args[0]); err != nil {
			return failErr(err)
		}
		st.DefaultImage = args[0]
	} else {
		return failErr(fmt.Errorf("usage: pvm image default [<distro:version>|--clear]"))
	}
	if err := saveDefaults(p, st); err != nil {
		return failErr(err)
	}
	if st.DefaultImage == "" {
		fmt.Println("cleared default image")
	} else {
		fmt.Printf("default image set to %s\n", st.DefaultImage)
	}
	return 0
}

// resolveVMName fills an omitted VM name from defaults.json, or from the only
// user VM. Destructive commands always require an explicit name.
func resolveVMName(p *paths.Paths, explicit string, destructive bool) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if destructive {
		return "", fmt.Errorf("vm name is required")
	}
	st := loadDefaults(p)
	if st.DefaultVM != "" {
		if _, err := vm.Load(p, st.DefaultVM); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("default vm %q no longer exists; set a new one with `pvm default <vm>` or clear it with `pvm default --clear`", st.DefaultVM)
			}
			return "", err
		}
		return st.DefaultVM, nil
	}
	specs, err := vm.ListAll(p)
	if err != nil {
		return "", err
	}
	var users []string
	for _, sp := range specs {
		if sp.Role != "gateway" && !strings.HasPrefix(sp.Name, "bake-") {
			users = append(users, sp.Name)
		}
	}
	sort.Strings(users)
	if len(users) == 1 {
		return users[0], nil
	}
	if len(users) == 0 {
		return "", fmt.Errorf("no user VMs yet; create one with `pvm create <name> --image <distro:version> --user <user>`")
	}
	return "", fmt.Errorf("multiple VMs exist (%s); pass a VM name or set `pvm default <vm>`", strings.Join(users, ", "))
}

// resolveVMNameFromArgs uses args[0] when it names an existing VM, otherwise
// resolves the default VM and leaves args intact as command arguments.
func resolveVMNameFromArgs(p *paths.Paths, args []string) (string, []string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if _, err := vm.Load(p, args[0]); err == nil {
			return args[0], args[1:], nil
		}
		if len(args) > 1 {
			return args[0], args[1:], nil
		}
		if specs, _ := vm.ListAll(p); len(specs) == 0 && loadDefaults(p).DefaultVM == "" {
			return args[0], args[1:], nil
		}
	}
	name, err := resolveVMName(p, "", false)
	return name, args, err
}

func resolveDefaultImage(p *paths.Paths) (string, error) {
	st := loadDefaults(p)
	if st.DefaultImage != "" {
		return st.DefaultImage, nil
	}
	goldens, err := image.ListGolden(p.Images)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, g := range goldens {
		seen[g.Distro+":"+g.ImageVersion] = true
	}
	if len(seen) == 1 {
		for k := range seen {
			return k, nil
		}
	}
	return "", fmt.Errorf("--image is required (e.g. --image ubuntu:26.04 or --image debian:13), or set `pvm image default <distro:version>`")
}

// ClearDefaultVMIfRemoved keeps defaults.json consistent after `pvm rm`.
func ClearDefaultVMIfRemoved(p *paths.Paths, name string) {
	st := loadDefaults(p)
	if st.DefaultVM == name {
		st.DefaultVM = ""
		_ = saveDefaults(p, st)
	}
}
