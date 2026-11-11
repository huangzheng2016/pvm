package cli

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/assets"
	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

var tcgWarningOnce sync.Once

func shouldWarnTCG(accel qemu.Accel, forced string) bool {
	return accel == qemu.AccelTCG && forced != "tcg"
}

func tcgHint(goos, wslDistro string) string {
	if goos == "windows" {
		return "warning: using TCG software emulation; for WHPX run `Enable-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform -All` in elevated PowerShell and reboot; for WSL2 set nestedVirtualization=true in %UserProfile%\\.wslconfig, run `wsl --shutdown`, verify /dev/kvm, and use PVM_BACKEND=wsl"
	}
	if goos == "linux" && wslDistro != "" {
		return "warning: using TCG software emulation in WSL2; add [wsl2] nestedVirtualization=true to %UserProfile%\\.wslconfig, run `wsl --shutdown`, restart WSL, and verify /dev/kvm"
	}
	return ""
}

func warnTCGAccel(accel qemu.Accel) {
	if !shouldWarnTCG(accel, os.Getenv("PVM_QEMU_ACCEL")) {
		return
	}
	hint := tcgHint(runtime.GOOS, os.Getenv("WSL_DISTRO_NAME"))
	if hint == "" {
		return
	}
	tcgWarningOnce.Do(func() { fmt.Fprintln(os.Stderr, hint) })
}

func warnTCG(c *daemon.Client) {
	var st struct {
		Accel qemu.Accel `json:"accel"`
	}
	if err := c.Get("/v1/status", &st); err == nil {
		warnTCGAccel(st.Accel)
	}
}

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
		for i := 0; i < 900 && c.Healthy(); i++ {
			if i > 0 && i%50 == 0 {
				fmt.Printf("  waiting for daemon to stop (%ds; vms shutting down in parallel)\n", i/5)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if c.Healthy() {
			return fatalf("daemon is still running after 180s; not stopped")
		}
		fmt.Println("daemon stopped")
		return 0
	case "restart":
		os.Remove(p.DaemonStopMark())
		c := daemon.NewClient(p)
		if c.Healthy() {
			if err := c.Post("/v1/shutdown", nil, nil); err != nil {
				return fatalf("shutdown: %v", err)
			}
			for i := 0; i < 900 && c.Healthy(); i++ {
				if i > 0 && i%50 == 0 {
					fmt.Printf("  waiting for old daemon to stop (%ds)\n", i/5)
				}
				time.Sleep(200 * time.Millisecond)
			}
			if c.Healthy() {
				return fatalf("old daemon is still running after 180s; refusing to start a second one")
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
		fmt.Printf("daemon: running\n  pid:     %v\n  version: %v\n  ready:   %v\n  uptime:  %v\n  accel:   %v\n  qemu:    %v (bundled=%v)\n  vms:     %v (%v running)\n",
			st["pid"], st["version"], st["ready"], st["uptime"], st["accel"], st["qemu"], st["bundled"], st["vms"], st["running"])
		if n, _ := st["tcg_fallback"].(float64); n > 0 {
			hint := "on macOS a reboot usually restores it"
			if runtime.GOOS == "windows" {
				hint = "enable HypervisorPlatform and reboot; for WSL2 set nestedVirtualization=true in .wslconfig and verify /dev/kvm"
			} else if runtime.GOOS == "linux" {
				hint = "check /dev/kvm permissions"
				if os.Getenv("WSL_DISTRO_NAME") != "" {
					hint = "set nestedVirtualization=true in .wslconfig, run `wsl --shutdown`, restart WSL, and verify /dev/kvm"
				}
			}
			fmt.Printf("  warning: %v vm(s) running under tcg fallback; host accel %v is unavailable (%s)\n", n, st["accel"], hint)
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

	tc, err := qemu.Find(p.Runtime)
	if err != nil {
		return fatalf("%v", err)
	}
	accel := qemu.Detect(tc, runtime.GOOS)
	clusters, err := cluster.LoadStore(p.Clusters)
	if err != nil {
		return fatalf("clusters: %v", err)
	}
	srv := daemon.NewServer(p, cfg, tc, accel, clusters)
	fmt.Fprintf(os.Stderr, "pvm daemon: qemu=%s accel=%s sock=%s\n", tc.System, accel, p.DaemonSock())
	if err := daemon.Run(p, srv); err != nil {
		return fatalf("daemon: %v", err)
	}
	return 0
}
