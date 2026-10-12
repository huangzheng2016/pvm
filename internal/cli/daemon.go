package cli

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/huangzheng2016/pvm/internal/assets"
	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/keys"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

func CmdDaemon(args []string) int {
	if len(args) == 0 {
		return fatalf("usage: pvm daemon run|start|stop|status|restart|install|uninstall")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	switch args[0] {
	case "run":
		return daemonRun(p)
	case "start":
		os.Remove(p.DaemonStopMark())
		if _, err := daemon.Ensure(p); err != nil {
			return fatalf("%v", err)
		}
		fmt.Println("daemon started")
		return 0
	case "stop":
		os.WriteFile(p.DaemonStopMark(), []byte("1\n"), 0o600)
		c := daemon.NewClient(p)
		if !c.Healthy() {
			fmt.Println("daemon not running")
			return 0
		}
		if err := c.Post("/v1/shutdown", nil, nil); err != nil {
			return fatalf("%v", err)
		}
		for i := 0; i < 150 && c.Healthy(); i++ {
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Println("daemon stopped")
		return 0
	case "restart":
		os.Remove(p.DaemonStopMark())
		c := daemon.NewClient(p)
		if c.Healthy() {
			c.Post("/v1/shutdown", nil, nil)
			for i := 0; i < 150 && c.Healthy(); i++ {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if _, err := daemon.Ensure(p); err != nil {
			return fatalf("%v", err)
		}
		fmt.Println("daemon restarted")
		return 0
	case "status":
		c := daemon.NewClient(p)
		if !c.Healthy() {
			fmt.Println("daemon: not running")
			return 0
		}
		var st map[string]any
		if err := c.Get("/v1/status", &st); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("daemon: running\n  pid:     %v\n  version: %v\n  uptime:  %v\n  accel:   %v\n  qemu:    %v (bundled=%v)\n  vms:     %v (%v running)\n",
			st["pid"], st["version"], st["uptime"], st["accel"], st["qemu"], st["bundled"], st["vms"], st["running"])
		if n, _ := st["tcg_fallback"].(float64); n > 0 {
			fmt.Printf("  warning: %v vm(s) running under tcg fallback; host accel %v is unavailable (on macOS a reboot usually restores it)\n", n, st["accel"])
		}
		return 0
	case "install":
		if err := daemon.ServiceInstall(p); err != nil {
			return fatalf("%v", err)
		}
		fmt.Println("service installed and started")
		return 0
	case "uninstall":
		if err := daemon.ServiceUninstall(p); err != nil {
			return fatalf("%v", err)
		}
		fmt.Println("service uninstalled")
		return 0
	}
	return fatalf("unknown daemon subcommand %q", args[0])
}

func daemonRun(p *paths.Paths) int {
	cfg, err := config.Load(p.ConfigFile())
	if err != nil {
		return fatalf("config: %v", err)
	}
	if err := assets.Unpack(p.Runtime); err != nil {
		return fatalf("assets: %v", err)
	}
	assets.CleanStaleVersions(p.Runtime)

	tc, err := qemu.Find(p.Runtime)
	if err != nil {
		return fatalf("%v", err)
	}
	accel := qemu.Detect(tc, runtime.GOOS)
	pub, err := keys.Ensure(p.SSHKey())
	if err != nil {
		return fatalf("ssh key: %v", err)
	}
	clusters, err := cluster.LoadStore(p.Clusters)
	if err != nil {
		return fatalf("clusters: %v", err)
	}
	srv := daemon.NewServer(p, cfg, tc, accel, pub, clusters)
	fmt.Fprintf(os.Stderr, "pvm daemon: qemu=%s accel=%s sock=%s\n", tc.System, accel, p.DaemonSock())
	if err := daemon.Run(p, srv); err != nil {
		return fatalf("daemon: %v", err)
	}
	return 0
}
