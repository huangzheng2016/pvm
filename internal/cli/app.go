package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/paths"
)

const appUsage = `[<vm>] <url> [--name ID] [--kiosk] [--no-attach] [--yes]
       pvm app [<vm>] [--name ID] [--no-attach] [--yes] -- <command> [args...]
       pvm app ls [<vm>]
       pvm app stop [<vm>] [<id> | --all]
       pvm app attach [<vm>] [--yes]
       pvm app config [<vm>] [--clipboard off|to-guest|to-host|both] [--drop on|off]`

type appView struct {
	VM    string   `json:"vm"`
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Argv  []string `json:"argv"`
	URL   string   `json:"url"`
	Kiosk bool     `json:"kiosk"`
	State string   `json:"state"`
}

type appAttach struct {
	Port         int    `json:"port"`
	PasswordFile string `json:"password_file"`
	Title        string `json:"title"`
	Clipboard    string `json:"clipboard"`
	Drop         bool   `json:"drop"`
	Connected    bool   `json:"connected"`
}

func CmdApp(args []string) int {
	if len(args) == 0 {
		return fatalf("usage: pvm app %s", appUsage)
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	switch args[0] {
	case "ls", "list":
		if len(args) > 2 {
			return fatalf("usage: pvm app ls [<vm>]")
		}
		c, err := ensureDaemon(p)
		if err != nil {
			return fatalf("%v", err)
		}
		path := "/v1/apps"
		if len(args) == 2 {
			path = "/v1/vms/" + args[1] + "/apps"
		}
		var apps []appView
		if err := c.Get(path, &apps); err != nil {
			return fatalf("%v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "VM\tID\tSTATE\tRUNS")
		for _, a := range apps {
			runs := a.URL
			if a.Kind == "cmd" {
				runs = strings.Join(a.Argv, " ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.VM, a.ID, a.State, runs)
		}
		tw.Flush()
		return 0
	case "stop":
		fs := newFlagSet("app stop", "[<vm>] [<id> | --all]")
		all := fs.Bool("all", false, "stop every app in the vm")
		if err := fs.Parse(partitionFlags(args[1:])); err != nil {
			return 2
		}
		if fs.NArg() > 2 || (fs.NArg() == 0 && !*all) || (fs.NArg() == 2 && *all) {
			return fatalf("usage: pvm app stop [<vm>] <id> | pvm app stop [<vm>] --all")
		}
		c, err := ensureDaemon(p)
		if err != nil {
			return fatalf("%v", err)
		}
		vmName, id := "", ""
		switch fs.NArg() {
		case 2:
			vmName, id = fs.Arg(0), fs.Arg(1)
		case 1:
			// one word is the vm with --all, otherwise the app id
			if *all {
				vmName = fs.Arg(0)
			} else {
				id = fs.Arg(0)
			}
		}
		if vmName, _, _, err = resolveVM(c, p, vmName); err != nil {
			return fatalf("%v", err)
		}
		path := "/v1/vms/" + vmName + "/apps"
		if id != "" {
			path += "/" + id
		}
		var out struct {
			Stopped []string `json:"stopped"`
		}
		if err := c.Delete(path, &out); err != nil {
			return fatalf("%v", err)
		}
		if len(out.Stopped) == 0 {
			fmt.Printf("no apps were running in %s\n", vmName)
		} else {
			fmt.Printf("stopped %s in %s\n", strings.Join(out.Stopped, ", "), vmName)
		}
		return 0
	case "attach":
		fs := newFlagSet("app attach", "[<vm>] [--yes]")
		yes := fs.Bool("yes", false, "start a stopped vm without asking")
		if err := fs.Parse(partitionFlags(args[1:])); err != nil {
			return 2
		}
		if fs.NArg() > 1 {
			return fatalf("usage: pvm app attach [<vm>] [--yes]")
		}
		c, err := ensureDaemon(p)
		if err != nil {
			return fatalf("%v", err)
		}
		vmName, _, _, err := resolveVM(c, p, fs.Arg(0))
		if err != nil {
			return fatalf("%v", err)
		}
		if err := vmUp(c, vmName, *yes); err != nil {
			return fatalf("%v", err)
		}
		var att appAttach
		if err := prepareApp(c, vmName); err != nil {
			return fatalf("%v", err)
		}
		if err := c.Post("/v1/vms/"+vmName+"/apps/attach?force=1", nil, &att); err != nil {
			return fatalf("%v", err)
		}
		if err := startXpraClient(vmName, att); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("xpra client started for %s\n", vmName)
		return 0
	case "config":
		const usage = "[<vm>] [--clipboard off|to-guest|to-host|both] [--drop on|off]"
		fs := newFlagSet("app config", usage)
		clip := fs.String("clipboard", "", "off, to-guest (host to vm), to-host (vm to host) or both")
		drop := fs.String("drop", "", "on: files dropped on the vm's windows upload into ~/Downloads in the vm")
		if err := fs.Parse(flagsFirst(fs, args[1:])); err != nil {
			return 2
		}
		if fs.NArg() > 1 || (*drop != "" && *drop != "on" && *drop != "off") {
			return fatalf("usage: pvm app config %s", usage)
		}
		body := map[string]any{}
		if *clip != "" {
			body["clipboard"] = *clip
		}
		if *drop != "" {
			body["drop"] = *drop == "on"
		}
		c, err := ensureDaemon(p)
		if err != nil {
			return fatalf("%v", err)
		}
		vmName, _, _, err := resolveVM(c, p, fs.Arg(0))
		if err != nil {
			return fatalf("%v", err)
		}
		var out struct {
			Clipboard string `json:"clipboard"`
			Drop      bool   `json:"drop"`
		}
		if err := c.Put("/v1/vms/"+vmName+"/apps/config", body, &out); err != nil {
			return fatalf("%v", err)
		}
		dropS := "off"
		if out.Drop {
			dropS = "on"
		}
		fmt.Printf("%s: clipboard %s, drop %s\n", vmName, out.Clipboard, dropS)
		if out.Clipboard == "to-guest" || out.Clipboard == "both" {
			fmt.Println("host clipboard content enters the vm when one of its windows comes to the front, and when you copy while it is there")
		}
		if out.Drop {
			fmt.Println("files dropped on its windows upload into ~/Downloads in the vm; files the vm sends need your confirmation")
		}
		if len(body) > 0 {
			fmt.Printf("it applies to clients attached from now on: pvm app attach %s\n", vmName)
		}
		return 0
	case "__clipboard":
		return cmdAppClipboardBridge(p, args[1:])
	}
	return cmdAppStart(p, args)
}

func cmdAppStart(p *paths.Paths, args []string) int {
	var cmd []string
	if i := indexOf(args, "--"); i >= 0 {
		args, cmd = args[:i], args[i+1:]
	}
	fs := newFlagSet("app", appUsage)
	name := fs.String("name", "", "app id (default: derived from the command or URL)")
	kiosk := fs.Bool("kiosk", false, "open the URL full screen")
	noAttach := fs.Bool("no-attach", false, "start the app without opening a host window")
	yes := fs.Bool("yes", false, "start a stopped vm without asking")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return 2
	}
	pos := fs.Args()
	body := map[string]any{"id": *name, "kiosk": *kiosk}
	vmName := ""
	switch {
	case len(pos) <= 1 && len(cmd) > 0:
		body["argv"] = cmd
		if len(pos) == 1 {
			vmName = pos[0]
		}
	case (len(pos) == 1 || len(pos) == 2) && len(cmd) == 0:
		target := pos[len(pos)-1]
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			if len(pos) == 1 {
				return fatalf("%q is not a URL; to run a command use: pvm app -- %s", target, target)
			}
			return fatalf("%q is not a URL; to run a command use: pvm app %s -- %s", target, pos[0], target)
		}
		body["url"] = target
		if len(pos) == 2 {
			vmName = pos[0]
		}
	default:
		return fatalf("usage: pvm app %s", appUsage)
	}
	client := ""
	if !*noAttach {
		// fail before starting anything in the guest
		var err error
		if client, err = findXpraClient(); err != nil {
			return fatalf("%v", err)
		}
	}
	c, err := ensureDaemon(p)
	if err != nil {
		return fatalf("%v", err)
	}
	if vmName, _, _, err = resolveVM(c, p, vmName); err != nil {
		return fatalf("%v", err)
	}
	if err := vmUp(c, vmName, *yes); err != nil {
		return fatalf("%v", err)
	}
	if err := prepareApp(c, vmName); err != nil {
		return fatalf("%v", err)
	}
	fmt.Fprintf(os.Stderr, "starting in %s...\n", vmName)
	var out struct {
		App    appView   `json:"app"`
		Attach appAttach `json:"attach"`
	}
	if err := c.Post("/v1/vms/"+vmName+"/apps", body, &out); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("started %s in %s\n", out.App.ID, vmName)
	switch {
	case *noAttach:
		fmt.Printf("open its window with: pvm app attach %s\n", vmName)
	case out.Attach.Connected:
		fmt.Println("its window opens in the running xpra client")
	default:
		if err := startXpraClientAt(vmName, client, out.Attach); err != nil {
			return fatalf("%v; the app runs, retry with `pvm app attach %s`", err, vmName)
		}
		fmt.Println("xpra client started; its window opens shortly")
	}
	return 0
}

// flagsFirst moves flags before positionals so flag.Parse sees them; a
// value flag takes the next word, a bool flag does not.
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if hasValue || f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(append(flags, "--"), pos...)
}

func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

// findXpraClient returns the host Xpra client: PVM_XPRA, the copy the
// Windows installer puts next to pvm, or a system install.
func findXpraClient() (string, error) {
	if p := os.Getenv("PVM_XPRA"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("PVM_XPRA: %w", err)
		}
		return p, xpraVersionOK(p)
	}
	var cands []string
	switch runtime.GOOS {
	case "windows":
		if exe, err := os.Executable(); err == nil {
			cands = append(cands, filepath.Join(filepath.Dir(exe), "xpra", "Xpra.exe"))
		}
		for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
			if d := os.Getenv(env); d != "" {
				cands = append(cands, filepath.Join(d, "Xpra", "Xpra.exe"))
			}
		}
	case "darwin":
		cands = append(cands, "/Applications/Xpra.app/Contents/MacOS/Xpra")
	default:
		if p, err := exec.LookPath("xpra"); err == nil {
			cands = append(cands, p)
		}
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			return p, xpraVersionOK(p)
		}
	}
	hint := map[string]string{
		"windows": "the Windows installers include it; or install the official client from https://xpra.org, or unpack its zip into an xpra folder next to pvm.exe",
		"darwin":  "install Xpra.app from https://xpra.org",
	}[runtime.GOOS]
	if hint == "" {
		hint = "install the xpra client from the xpra.org repository"
	}
	return "", fmt.Errorf("no Xpra %s.x client found; %s (or set PVM_XPRA to its path)", image.XpraVersion, hint)
}

// xpraVersionOK requires the client's release line to match the guest's.
func xpraVersionOK(client string) error {
	bin := client
	if runtime.GOOS == "windows" {
		// Xpra.exe is a GUI program; the console twin prints the version
		if cmd := filepath.Join(filepath.Dir(client), "Xpra_cmd.exe"); fileExists(cmd) {
			bin = cmd
		}
	}
	c := exec.Command(bin, "--version")
	out, err := c.Output()
	if err != nil {
		return fmt.Errorf("%s --version: %v", bin, err)
	}
	v := strings.TrimSpace(string(out))
	if i := strings.LastIndex(v, "\n"); i >= 0 {
		v = v[i+1:]
	}
	if !strings.HasPrefix(v, "xpra v"+image.XpraVersion+".") {
		return fmt.Errorf("%s is %q; pvm app needs Xpra %s.x on host and guest", client, v, image.XpraVersion)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func startXpraClient(vm string, att appAttach) error {
	client, err := findXpraClient()
	if err != nil {
		return err
	}
	return startXpraClientAt(vm, client, att)
}

// startXpraClientAt starts the client detached; it outlives the CLI.
// Xpra's own clipboard only carries guest content to the host; host
// content goes in through the clipboard bridge, which sends it only while
// one of the VM's windows is in front.
func startXpraClientAt(vm, client string, att appAttach) error {
	toGuest := att.Clipboard == "to-guest" || att.Clipboard == "both"
	if toGuest && !clipboardBridgeSupported {
		return fmt.Errorf("the host clipboard cannot enter the vm from this host yet; use `pvm app config %s --clipboard to-host`", vm)
	}
	clip := []string{"--clipboard=no"}
	if att.Clipboard == "to-host" || att.Clipboard == "both" {
		clip = []string{"--clipboard=yes", "--clipboard-direction=to-client"}
	}
	args := append([]string{
		"attach", fmt.Sprintf("tcp://127.0.0.1:%d/", att.Port),
		"--password-file=" + att.PasswordFile,
		"--title=" + att.Title,
	}, clip...)
	// file transfer flows both ways in Xpra; ask lets dropped files upload
	// and makes the user confirm each file the guest sends
	files := "--file-transfer=no"
	if att.Drop {
		files = "--file-transfer=ask"
	}
	args = append(args, files,
		"--open-files=no", "--open-url=no",
		"--printing=no", "--speaker=off", "--microphone=off", "--webcam=no",
		"--notifications=no", "--tray=no", "--splash=no", "--reconnect=yes",
		// the client's defaults win over the server's: keep encoding fast
		"--min-speed=80", "--min-quality=50",
	)
	if runtime.GOOS == "windows" {
		// OpenGL painting on Windows slows the client's frame acks, and the
		// server paces updates by them: a full-window animation ran at 22
		// fps with it and 50 fps without, at a few percent of one core
		args = append(args, "--opengl=no")
	}
	c := exec.Command(client, args...)
	logPath := filepath.Join(filepath.Dir(att.PasswordFile), "client.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err == nil {
		c.Stdout, c.Stderr = f, f
		defer f.Close()
	}
	if runtime.GOOS == "windows" {
		// the GUI client writes no console output, only its own log
		logPath = filepath.Join(os.Getenv("LOCALAPPDATA"), "Xpra", "Xpra.log")
	}
	detachClient(c)
	if err := c.Start(); err != nil {
		return fmt.Errorf("start xpra client: %w", err)
	}
	// an immediate exit means bad arguments or a refused connection
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return fmt.Errorf("xpra client exited right away (%v); see %s", err, logPath)
	case <-time.After(3 * time.Second):
	}
	if toGuest {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		b := exec.Command(self, "app", "__clipboard", vm, strconv.Itoa(c.Process.Pid), filepath.Dir(client))
		detachClient(b)
		if err := b.Start(); err != nil {
			return fmt.Errorf("start clipboard bridge: %w", err)
		}
		b.Process.Release()
	}
	if att.Clipboard == "off" {
		// otherwise copy and paste just do nothing, with no hint why
		fmt.Fprintf(os.Stderr, "clipboard is off for %s; turn it on with `pvm app config %s --clipboard both` and reattach\n", vm, vm)
	}
	return nil
}

// cmdAppClipboardBridge runs until the Xpra client exits; see
// runClipboardBridge.
func cmdAppClipboardBridge(p *paths.Paths, args []string) int {
	if len(args) != 3 {
		return fatalf("usage: pvm app __clipboard <vm> <client-pid> <client-dir>")
	}
	pid, err := strconv.Atoi(args[1])
	if err != nil {
		return fatalf("bad pid %q", args[1])
	}
	c := daemon.NewClient(p)
	open := func(kind string) (io.ReadWriteCloser, error) {
		return c.Upgrade("/v1/vms/" + args[0] + "/stream?target=clip:" + kind)
	}
	if err := runClipboardBridge(args[0], pid, args[2], func(cc clipContent) error { return sendClipboard(open, cc) }); err != nil {
		return fatalf("%v", err)
	}
	return 0
}

// prepareApp installs Xpra in the VM on first use, showing the
// installer's output as it runs.
func prepareApp(c *daemon.Client, vm string) error {
	resp, err := c.PostStream("/v1/vms/"+vm+"/apps/prepare", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	started := false
	for {
		var f struct {
			Out       string `json:"out"`
			Done      bool   `json:"done"`
			Installed bool   `json:"installed"`
			Error     string `json:"error"`
		}
		if err := dec.Decode(&f); err != nil {
			return fmt.Errorf("install xpra in %s: %w", vm, err)
		}
		if f.Out != "" {
			if !started {
				fmt.Fprintf(os.Stderr, "installing xpra in %s (first use, a few minutes)...\n", vm)
				started = true
			}
			fmt.Fprintf(os.Stderr, "        | %s\n", f.Out)
		}
		if f.Done {
			if f.Error != "" {
				return fmt.Errorf("%s", f.Error)
			}
			if f.Installed {
				fmt.Fprintf(os.Stderr, "xpra installed in %s\n", vm)
			}
			return nil
		}
	}
}
