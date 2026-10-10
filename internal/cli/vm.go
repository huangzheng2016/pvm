package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/image"
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
}

func CmdImage(args []string) int {
	if len(args) == 0 {
		return fatalf("usage: pvm image fetch <distro:version> | pvm image list")
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
		ref, err := image.Resolve(args[1], runtime.GOARCH)
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
	case "list":
		imgs, err := image.List(p.Images)
		if err != nil {
			return fatalf("%v", err)
		}
		if len(imgs) == 0 {
			fmt.Println("no images; run `pvm image fetch ubuntu:24.04`")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSIZE")
		for _, im := range imgs {
			fmt.Fprintf(tw, "%s\t%d MB\n", im.Name, im.Size>>20)
		}
		tw.Flush()
		return 0
	}
	return fatalf("unknown image subcommand %q", args[0])
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
	var out vmInfoView
	if err := c.Post("/v1/vms", body, &out); err != nil {
		return fatalf("create: %v", err)
	}
	fmt.Printf("created %s (user=%s ip=%s)\n", out.Spec.Name, out.Spec.User, out.Spec.IP)
	if !*noStart {
		fmt.Printf("starting; first boot runs cloud-init (packages, /share mount) and may take a few minutes\n")
	}
	return 0
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
	switch action {
	case "start", "stop":
		if err := c.Post(fmt.Sprintf("/v1/vms/%s/%s", name, action), nil, nil); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("%s %s\n", action, name)
	case "rm":
		if err := c.Delete("/v1/vms/"+name, nil); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("removed %s\n", name)
	}
	return 0
}

func CmdStart(args []string) int { return cmdVMSimple(args, "start") }
func CmdStop(args []string) int  { return cmdVMSimple(args, "stop") }
func CmdRm(args []string) int    { return cmdVMSimple(args, "rm") }

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
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return fatalf("ssh: %v", err)
	}
	return 0
}
