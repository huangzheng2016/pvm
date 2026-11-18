package desktop

import (
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/workspace"
)

const (
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateReady    = "ready"
	stateFailed   = "failed"

	direct = "direct"
)

// vmInfo is the part of a GET /v1/vms entry the app uses.
type vmInfo struct {
	Spec struct {
		Name   string `json:"name"`
		Image  string `json:"image"`
		Role   string `json:"role"`
		CPUs   int    `json:"cpus"`
		MemMB  int    `json:"mem_mb"`
		DiskGB int    `json:"disk_gb"`
	} `json:"spec"`
	Running bool `json:"running"`
	Ready   bool `json:"ready"`
	Stage   struct {
		Stage  string `json:"stage"`
		Detail string `json:"detail"`
	} `json:"stage"`
}

// vmState is the rule `pvm ls` uses. A running VM without a stage, as
// after a daemon restart, is starting; so is one whose last stage is
// ready while an identity reset is still pending.
func vmState(in vmInfo) (state, stage, hint string) {
	switch {
	case !in.Running:
		return stateStopped, "", ""
	case in.Ready:
		return stateReady, "", ""
	case in.Stage.Stage == "failed":
		return stateFailed, "", in.Stage.Detail
	case in.Stage.Stage == "ready":
		return stateStarting, "", ""
	}
	return stateStarting, in.Stage.Stage, ""
}

// routes is the routing state of GET /v1/clusters.
type routes struct {
	VMExits     map[string]string `json:"vm_exits"`
	DefaultExit string            `json:"default_exit"`
	Rules       []struct {
		Type    string `json:"type"`
		Pattern string `json:"pattern"`
		Exit    string `json:"exit"`
	} `json:"rules"`
}

// readRoutes returns the one cluster pvm keeps; without a cluster yet
// every VM is direct.
func (a *App) readRoutes() (routes, error) {
	var clusters []routes
	if err := a.daemon.Get("/v1/clusters", &clusters); err != nil {
		return routes{}, err
	}
	if len(clusters) == 0 {
		return routes{}, nil
	}
	return clusters[0], nil
}

func (r routes) defaultExit() string {
	if r.DefaultExit == "" {
		return direct
	}
	return r.DefaultExit
}

// exitOf returns a VM's effective exit and whether it has its own.
func (r routes) exitOf(vm string) (name string, own bool) {
	if e := r.VMExits[vm]; e != "" {
		return e, true
	}
	return r.defaultExit(), false
}

func (a *App) readVMs() ([]vmInfo, error) {
	var infos []vmInfo
	if err := a.daemon.Get("/v1/vms", &infos); err != nil {
		return nil, err
	}
	return infos, nil
}

// VM is one row of the VM list.
type VM struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"` // stopped, starting, ready or failed
	// Stage is the current stage of a starting VM, when it has one.
	Stage string `json:"stage"`
	// Hint says what to do about a failed VM.
	Hint   string `json:"hint"`
	CPUs   int    `json:"cpus"`
	MemMB  int    `json:"mem_mb"`
	DiskGB int    `json:"disk_gb"`
	Exit   string `json:"exit"` // an exit name, or direct
	// ExitOwn is false when the VM follows the default exit.
	ExitOwn bool `json:"exit_own"`
	Default bool `json:"default"`
}

// VMs lists the user VMs; gateways are hidden.
func (a *App) VMs() ([]VM, error) {
	if err := a.native(); err != nil {
		return nil, err
	}
	infos, err := a.readVMs()
	if err != nil {
		return nil, err
	}
	rt, err := a.readRoutes()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(a.p.ConfigFile())
	if err != nil {
		return nil, err
	}
	list := []VM{}
	for _, in := range infos {
		if in.Spec.Role == "gateway" {
			continue
		}
		v := VM{
			Name: in.Spec.Name, Image: in.Spec.Image,
			CPUs: in.Spec.CPUs, MemMB: in.Spec.MemMB, DiskGB: in.Spec.DiskGB,
			Default: in.Spec.Name == cfg.DefaultVM,
		}
		v.State, v.Stage, v.Hint = vmState(in)
		v.Exit, v.ExitOwn = rt.exitOf(v.Name)
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// CatalogApp is one program of the built-in catalog.
type CatalogApp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
	Source      string `json:"source"` // official or community
	// Window is false for a program that runs in a terminal.
	Window bool   `json:"window"`
	Start  string `json:"start"`
}

// Catalog lists the programs `pvm app install` knows, as the daemon
// reports them: the app may be older than the pvm it sits next to.
func (a *App) Catalog() ([]CatalogApp, error) {
	if err := a.native(); err != nil {
		return nil, err
	}
	return a.readCatalog()
}

func (a *App) readCatalog() ([]CatalogApp, error) {
	list := []CatalogApp{}
	if err := a.daemon.Get("/v1/apps/catalog", &list); err != nil {
		return nil, err
	}
	return list, nil
}

// InstalledApps lists the catalog programs installed in one VM. It is
// not part of VMs because the daemon asks the guest about running apps
// to answer it, which is too much for every poll.
func (a *App) InstalledApps(vm string) ([]CatalogApp, error) {
	if err := a.native(); err != nil {
		return nil, err
	}
	if err := checkVM(vm); err != nil {
		return nil, err
	}
	var resp struct {
		Installed map[string][]string `json:"installed"`
	}
	if err := a.daemon.Get("/v1/vms/"+url.PathEscape(vm)+"/apps", &resp); err != nil {
		return nil, err
	}
	catalog, err := a.readCatalog()
	if err != nil {
		return nil, err
	}
	list := []CatalogApp{}
	for _, c := range catalog {
		if slices.Contains(resp.Installed[vm], c.ID) {
			list = append(list, c)
		}
	}
	return list, nil
}

// Exit is one exit node. Tokens, keys and addresses inside the tunnel
// stay in the backend.
type Exit struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // agent (pvm exit-agent over WireGuard) or warp
	Endpoint string `json:"endpoint"`
	TZ       string `json:"tz"`
	Locale   string `json:"locale"`
	Default  bool   `json:"default"`
	// VMs are the VMs whose traffic leaves through this exit, by their own
	// route or by following the default.
	VMs []string `json:"vms"`
	// Rules are the split rules that name this exit.
	Rules []string `json:"rules"`
}

// DefaultExit is the exit VMs without their own follow: direct or a
// name, which may be that of an exit that was removed.
func (a *App) DefaultExit() (string, error) {
	if err := a.native(); err != nil {
		return "", err
	}
	rt, err := a.readRoutes()
	if err != nil {
		return "", err
	}
	return rt.defaultExit(), nil
}

func (a *App) exitsDir() string { return filepath.Join(a.p.Home, "exits") }

// Exits lists the exits with who uses each.
func (a *App) Exits() ([]Exit, error) {
	if err := a.native(); err != nil {
		return nil, err
	}
	entries, err := exit.ListEntries(a.exitsDir())
	if err != nil {
		return nil, err
	}
	infos, err := a.readVMs()
	if err != nil {
		return nil, err
	}
	rt, err := a.readRoutes()
	if err != nil {
		return nil, err
	}
	list := []Exit{}
	for _, e := range entries {
		list = append(list, exitView(e, infos, rt))
	}
	return list, nil
}

func exitView(e *exit.Entry, infos []vmInfo, rt routes) Exit {
	v := Exit{
		Name: e.Name, Kind: "agent", Endpoint: e.Endpoint,
		TZ: e.TZ, Locale: e.Locale,
		Default: rt.DefaultExit == e.Name,
		VMs:     []string{}, Rules: []string{},
	}
	if e.IsWARP() {
		v.Kind = "warp"
	}
	if v.TZ == "" && e.AutoTZ != "" {
		v.TZ = e.AutoTZ + "(auto)"
	}
	for _, in := range infos {
		if in.Spec.Role == "gateway" {
			continue
		}
		if name, _ := rt.exitOf(in.Spec.Name); name == e.Name {
			v.VMs = append(v.VMs, in.Spec.Name)
		}
	}
	// a route can outlive the list entry of its VM
	for vm, name := range rt.VMExits {
		if name == e.Name && !slices.Contains(v.VMs, vm) {
			v.VMs = append(v.VMs, vm)
		}
	}
	sort.Strings(v.VMs)
	for _, r := range rt.Rules {
		if r.Exit == e.Name {
			v.Rules = append(v.Rules, r.Type+" "+r.Pattern)
		}
	}
	return v
}

// Workspace is one host folder bound to a VM.
type Workspace struct {
	ID        string `json:"id"`
	VM        string `json:"vm"`
	Mode      string `json:"mode"` // mount, sync or copy
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only"`
	// LastTransfer is RFC 3339, or empty when nothing was transferred.
	LastTransfer string `json:"last_transfer"`
}

func (a *App) Workspaces() ([]Workspace, error) {
	if err := a.native(); err != nil {
		return nil, err
	}
	all, err := workspace.NewStore(a.p).List()
	if err != nil {
		return nil, err
	}
	list := []Workspace{}
	for _, ws := range all {
		v := Workspace{
			ID: ws.ID, VM: ws.VM, Mode: string(ws.Mode),
			HostPath: ws.HostPath, GuestPath: ws.GuestPath, ReadOnly: ws.ReadOnly,
		}
		if !ws.LastTransfer.IsZero() {
			v.LastTransfer = ws.LastTransfer.Format(time.RFC3339)
		}
		list = append(list, v)
	}
	return list, nil
}

// Settings is the state of the Settings screen.
type Settings struct {
	DefaultVM string `json:"default_vm"`
	// DaemonAtLogin is whether the pvm-daemon scheduled task exists.
	DaemonAtLogin bool `json:"daemon_at_login"`
	AppAtLogin    bool `json:"app_at_login"`
}

func (a *App) Settings() (Settings, error) {
	if err := a.native(); err != nil {
		return Settings{}, err
	}
	cfg, err := config.Load(a.p.ConfigFile())
	if err != nil {
		return Settings{}, err
	}
	s := Settings{DefaultVM: cfg.DefaultVM}
	if s.DaemonAtLogin, err = a.sys.daemonAtLogin(); err != nil {
		return Settings{}, err
	}
	if s.AppAtLogin, err = a.sys.appAtLogin(); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// Image is one choice of the Create form.
type Image struct {
	Spec string `json:"spec"`
	// Golden is true when a baked image is cached, so create needs no
	// download or bake.
	Golden bool `json:"golden"`
}

func (a *App) Images() ([]Image, error) {
	if err := a.native(); err != nil {
		return nil, err
	}
	list := []Image{}
	for _, spec := range image.Supported() {
		distro, version, _ := strings.Cut(spec, ":")
		g, err := image.LatestGolden(a.p.Images, distro, version)
		if err != nil {
			return nil, fmt.Errorf("read golden image catalog: %w", err)
		}
		list = append(list, Image{Spec: spec, Golden: g != nil})
	}
	return list, nil
}

// CreateDefaults fills the Create form.
type CreateDefaults struct {
	User   string `json:"user"`
	CPUs   int    `json:"cpus"`
	MemMB  int    `json:"mem_mb"`
	DiskGB int    `json:"disk_gb"`
	// Exit is the default exit a new VM follows, or direct.
	Exit string `json:"exit"`
}

// CreateDefaults reads config.yaml as the CLI does. A running daemon
// keeps the values it loaded at its start, so create always passes the
// form's numbers instead of relying on either.
func (a *App) CreateDefaults() (CreateDefaults, error) {
	if err := a.native(); err != nil {
		return CreateDefaults{}, err
	}
	cfg, err := config.Load(a.p.ConfigFile())
	if err != nil {
		return CreateDefaults{}, err
	}
	rt, err := a.readRoutes()
	if err != nil {
		return CreateDefaults{}, err
	}
	return CreateDefaults{
		User: "pvm",
		CPUs: cfg.Defaults.CPUs, MemMB: cfg.Defaults.MemMB, DiskGB: cfg.Defaults.DiskGB,
		Exit: rt.defaultExit(),
	}, nil
}
