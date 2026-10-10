package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/keys"
	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

type vmInfoView struct {
	Spec struct {
		Name    string `json:"name"`
		Image   string `json:"image"`
		Role    string `json:"role"`
		User    string `json:"user"`
		SSHPort int    `json:"ssh_port"`
		IP      string `json:"ip"`
	} `json:"spec"`
	Running     bool   `json:"running"`
	PID         int    `json:"pid"`
	Uptime      string `json:"uptime"`
	GatewaySSH  int    `json:"gw_ssh_port"`
	GatewayUser string `json:"gw_user"`
	Ready       bool   `json:"ready"`
	Stages      []struct {
		Stage  string `json:"stage"`
		Detail string `json:"detail"`
	} `json:"stages"`
}

func CmdImage(args []string) int {
	if len(args) == 0 {
		return fatalf("usage: pvm image fetch <distro:version> | pvm image build <distro:version> [--keep-vm] | pvm image list")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	switch args[0] {
	case "fetch":
		if len(args) < 2 {
			return fatalf("usage: pvm image fetch <distro:version>")
		}
		ref, err := image.Resolve(args[1])
		if err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("fetching %s\n  %s\n", ref.FileName(), ref.URL)
		path, err := image.Fetch(ref, p.Images, os.Stderr)
		if err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("ok: %s\n", path)
		return 0
	case "build":
		fs := newFlagSet("image build", "<distro:version> [--keep-vm] [--cpus 4] [--mem 6144] [--disk 25]")
		keep := fs.Bool("keep-vm", false, "keep the build VM for debugging")
		cpus := fs.Int("cpus", 4, "build vm vcpus")
		mem := fs.Int("mem", 6144, "build vm memory MB")
		disk := fs.Int("disk", 25, "build vm disk GB")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() < 1 {
			fs.Usage()
			return 2
		}
		return cmdImageBuild(p, fs.Arg(0), *keep, *cpus, *mem, *disk)
	case "list":
		imgs, err := image.List(p.Images)
		if err != nil {
			return fatalf("%v", err)
		}
		goldens := image.ListGolden(p.Images)
		if len(imgs) == 0 && len(goldens) == 0 {
			fmt.Println("no images; run `pvm image fetch ubuntu:24.04`")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tSIZE")
		for _, im := range imgs {
			fmt.Fprintf(tw, "%s\tbase\t%d MB\n", im.Name, im.Size>>20)
		}
		for _, g := range goldens {
			path := p.Images + "/golden/" + g.File
			var size int64
			if fi, err := os.Stat(path); err == nil {
				size = fi.Size()
			}
			fmt.Fprintf(tw, "%s\tgolden\t%d MB\n", g.File, size>>20)
		}
		tw.Flush()
		return 0
	}
	return fatalf("unknown image subcommand %q", args[0])
}

func cmdImageBuild(p *paths.Paths, spec string, keep bool, cpus, mem, disk int) int {
	tc, err := qemu.Find(p.Runtime)
	if err != nil {
		return fatalf("%v", err)
	}
	accel := qemu.Detect(tc, runtime.GOOS)
	pub, err := keys.Ensure(p.SSHKey())
	if err != nil {
		return fatalf("ssh key: %v", err)
	}
	entry, err := image.Bake(&image.BakeOptions{
		Spec: spec, P: p, TC: tc, Accel: accel,
		SSHKey: p.SSHKey(), SSHPub: pub, Out: os.Stderr,
		KeepVM: keep, BuildCPUs: cpus, BuildMem: mem, BuildDisk: disk,
	})
	if err != nil {
		return fatalf("image build: %v", err)
	}
	fmt.Printf("built golden %s v%d (%s)\n  docker: %s\n",
		spec, entry.Version, entry.File, strings.Join(entry.DockerImages, ", "))
	return 0
}

func CmdCreate(args []string) int {
	fs := newFlagSet("create", "--image ubuntu:24.04 --name dev1 [--user U] [--cpus 2] [--mem 4096] [--disk 20] [--mount /host:/guest]... [--no-start]")
	imageSpec := fs.String("image", "", "image spec, e.g. ubuntu:24.04")
	name := fs.String("name", "", "vm name")
	user := fs.String("user", "", "guest username (interactive prompt if empty)")
	cpus := fs.Int("cpus", 0, "vcpus")
	mem := fs.Int("mem", 0, "memory MB")
	disk := fs.Int("disk", 0, "disk GB")
	noStart := fs.Bool("no-start", false, "create but do not start")
	noWait := fs.Bool("no-wait", false, "return immediately after boot instead of following provisioning")
	tz := fs.String("tz", "", "guest timezone (IANA, e.g. America/Los_Angeles; default UTC)")
	locale := fs.String("locale", "", "guest locale (default en_US.UTF-8)")
	gitEmail := fs.String("git-email", "", "git user.email in the guest (never copied from the host)")
	gitName := fs.String("git-name", "", "git user.name in the guest")
	smbiosSpoof := fs.Bool("smbios-spoof", false, "present commodity PC hardware in DMI instead of QEMU defaults")
	var mounts multiFlag
	fs.Var(&mounts, "mount", "extra /host:/guest mount (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *imageSpec == "" || *name == "" {
		fs.Usage()
		return 2
	}
	if *user == "" {
		*user = promptUsername()
	}

	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	body := map[string]any{
		"name": *name, "image": *imageSpec, "user": *user,
		"cpus": *cpus, "mem_mb": *mem, "disk_gb": *disk,
		"mounts": []string(mounts), "start": !*noStart,
	}
	if *tz != "" {
		body["tz"] = *tz
	}
	if *locale != "" {
		body["locale"] = *locale
	}
	if *gitEmail != "" {
		body["git_email"] = *gitEmail
	}
	if *gitName != "" {
		body["git_name"] = *gitName
	}
	if *smbiosSpoof {
		body["smbios_spoof"] = true
	}
	var out vmInfoView
	describeImage(*imageSpec, p)
	start := time.Now()
	if err := c.Post("/v1/vms", body, &out); err != nil {
		return fatalf("create: %v", err)
	}
	fmt.Printf("[2/5] disk+seed: overlay + cloud-init (done in %.1fs)\n", time.Since(start).Seconds())
	fmt.Printf("[3/5] cluster: ip=%s via gateway\n[4/5] boot: qemu started\n", out.Spec.IP)
	fmt.Printf("created %s (user=%s ip=%s)\n", out.Spec.Name, out.Spec.User, out.Spec.IP)
	if *noStart {
		return 0
	}
	if *noWait {
		fmt.Printf("provisioning in background; watch: pvm logs %s --follow\n", *name)
		return 0
	}
	fmt.Printf("[5/5] provision: following (ctrl-c to detach; vm keeps provisioning)\n")
	if rc := followProvision(c, *name, 15*time.Minute); rc != 0 {
		return rc
	}
	fmt.Printf("ready in %.0fs; ssh: pvm ssh %s\n", time.Since(start).Seconds(), *name)
	return 0
}

// describeImage prints the [1/5] stage line: which base/golden image
// the VM will come from, or a fetch hint when it is missing.
func describeImage(spec string, p *paths.Paths) {
	kind := "base"
	file := ""
	var size int64
	distro, version, ok := strings.Cut(spec, ":")
	if ok {
		if g := image.LatestGolden(p.Images, distro, version); g != nil {
			kind = fmt.Sprintf("golden v%d", g.Version)
			file = g.File
			if fi, err := os.Stat(p.Images + "/golden/" + g.File); err == nil {
				size = fi.Size()
			}
		}
	}
	if file == "" {
		stem := strings.ReplaceAll(spec, ":", "-") + "-" + native.Arch + ".qcow2"
		file = stem
		if fi, err := os.Stat(p.Images + "/" + stem); err == nil {
			size = fi.Size()
		} else {
			fmt.Printf("[1/5] image: %s (MISSING - fetch first: pvm image fetch %s)\n", spec, spec)
			return
		}
	}
	fmt.Printf("[1/5] image: %s (%s, %.1fG)\n", spec, kind, float64(size)/1e9)
}

func promptUsername() string {
	fmt.Print("guest username [pvm]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return "pvm"
	}
	return line
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func CmdList(args []string) int {
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var infos []vmInfoView
	if err := c.Get("/v1/vms", &infos); err != nil {
		return fatalf("%v", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tROLE\tSTATUS\tIP\tUSER\tUPTIME")
	for _, in := range infos {
		status := "stopped"
		if in.Running {
			status = "running"
		}
		ip := in.Spec.IP
		if ip == "" {
			ip = "-"
		}
		role := in.Spec.Role
		if role == "" {
			role = "user"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			in.Spec.Name, in.Spec.Image, role, status, ip, in.Spec.User, in.Uptime)
	}
	tw.Flush()
	return 0
}

func cmdVMSimple(args []string, action string) int {
	if len(args) < 1 {
		return fatalf("usage: pvm %s <name>", action)
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	name := args[0]
	if err := c.Post(fmt.Sprintf("/v1/vms/%s/%s", name, action), nil, nil); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("%s %s\n", action, name)
	return 0
}

func CmdStart(args []string) int { return cmdVMSimple(args, "start") }
func CmdStop(args []string) int  { return cmdVMSimple(args, "stop") }

func CmdRm(args []string) int {
	fs := newFlagSet("rm", "<name> [-y|--force]")
	yes := fs.Bool("y", false, "confirm deletion (non-interactive)")
	force := fs.Bool("force", false, "alias for -y")
	if err := fs.Parse(partitionFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	if !*yes && !*force {
		if !stdinIsTTY() {
			return fatalf("refusing to delete %q without confirmation (non-interactive stdin); use -y to confirm", name)
		}
		if !confirmAction(os.Stdin, os.Stderr, fmt.Sprintf("delete VM '%s' (disk + metadata)?", name)) {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	if err := c.Delete("/v1/vms/"+name, nil); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("removed %s\n", name)
	return 0
}

func CmdSSH(args []string) int {
	if len(args) < 1 {
		return fatalf("usage: pvm ssh <name> [command...]")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var info vmInfoView
	if err := c.Get("/v1/vms/"+args[0], &info); err != nil {
		return fatalf("%v", err)
	}
	if !info.Running {
		return fatalf("vm %s is not running; `pvm start %s`", info.Spec.Name, info.Spec.Name)
	}

	sshArgs := []string{
		"-i", p.SSHKey(),
		// host keys rotate freely (fresh VMs, reused IPs/ports)
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	target := ""
	if info.Spec.Role == "gateway" {
		sshArgs = append(sshArgs, "-p", strconv.Itoa(info.Spec.SSHPort))
		target = fmt.Sprintf("%s@127.0.0.1", info.Spec.User)
	} else {
		// user VMs live behind the cluster gateway. -J would spawn the
		// hop ssh WITHOUT our flags (default known_hosts, no key), so
		// build the ProxyCommand by hand.
		proxy := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -W %%h:%%p -p %d %s@127.0.0.1",
			p.SSHKey(), info.GatewaySSH, info.GatewayUser)
		sshArgs = append(sshArgs, "-o", "ProxyCommand="+proxy)
		target = fmt.Sprintf("%s@%s", info.Spec.User, info.Spec.IP)
	}
	if len(args) == 1 {
		sshArgs = append(sshArgs, "-tt")
	}
	sshArgs = append(sshArgs, target)
	sshArgs = append(sshArgs, args[1:]...)

	cmd := exec.Command("ssh", sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// Connection-level failures (exit 255 with a dial error) fall
		// back to the virtio-serial agent channel; remote command
		// failures just propagate their exit code.
		if isSSHConnFailure(err, errBuf.String()) {
			fmt.Fprintf(os.Stderr, "ssh unreachable (%s), using agent channel\n", firstLine(errBuf.String()))
			execArgs := append([]string{args[0]}, args[1:]...)
			if len(execArgs) == 1 {
				fmt.Fprintln(os.Stderr, "note: agent channel has no TTY; starting a line-based bash")
				execArgs = append(execArgs, "bash", "-l")
			}
			return CmdExec(execArgs)
		}
		os.Stderr.Write(errBuf.Bytes())
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return fatalf("ssh: %v", err)
	}
	return 0
}

func isSSHConnFailure(err error, stderr string) bool {
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 255 {
		return false
	}
	s := strings.ToLower(stderr)
	for _, pat := range []string{"connection refused", "connection timed out", "operation timed out", "no route to host", "connection closed", "connection reset", "stdio forwarding failed", "open failed", "administratively prohibited"} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
