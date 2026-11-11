package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

// ResolveWindowsBackend returns the effective Windows execution backend:
// "native" or "wsl". On non-Windows hosts it always returns "native".
//
// Selection order on Windows:
//  1. PVM_BACKEND env ("native" | "wsl" | "auto")
//  2. config.yaml backend ("native" | "wsl" | "auto")
//  3. In "auto" mode (default):
//     - If a native Windows daemon is already healthy, or native QEMU is
//     installed, use "native".
//     - Otherwise, if WSL2 is available and has `pvm` installed, use "wsl".
//     - Otherwise default to "native".
func ResolveWindowsBackend(p *paths.Paths) (string, error) {
	if runtime.GOOS != "windows" {
		return "native", nil
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("PVM_BACKEND")))
	if mode == "" && p != nil {
		if cfg, err := config.Load(p.ConfigFile()); err == nil && cfg.Backend != "" {
			mode = strings.ToLower(strings.TrimSpace(cfg.Backend))
		}
	}
	switch mode {
	case "", "auto":
		if p != nil && daemon.NewClient(p).Healthy() {
			return "native", nil
		}
		if p != nil {
			if _, err := qemu.Find(p.Runtime); err == nil {
				return "native", nil
			}
		}
		if wslHasPVM() {
			return "wsl", nil
		}
		return "native", nil
	case "native", "wsl":
		return mode, nil
	default:
		return "", fmt.Errorf("invalid backend %q (want auto, native, or wsl)", mode)
	}
}

// ShouldDelegateToWSL reports whether a CLI invocation on Windows should be
// forwarded to `pvm` inside WSL2.
func ShouldDelegateToWSL(args []string) (bool, error) {
	if runtime.GOOS != "windows" || len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "version", "--version", "-v", "skill", "security", "doctor", "upgrade", "exit-agent", "agent", "gateway", "__snapshot-pipe-out", "__snapshot-pipe-in":
		return false, nil
	}
	p, err := paths.Default()
	if err != nil {
		return false, nil
	}
	backend, err := ResolveWindowsBackend(p)
	if err != nil {
		return false, err
	}
	return backend == "wsl", nil
}

// DelegateToWSL executes `pvm` inside WSL2, translating Windows host mount
// paths (e.g. C:\Users\dev\proj:/home/dev/proj) into /mnt/<drive>/... paths.
func DelegateToWSL(args []string) int {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return fatalf("WSL backend selected, but wsl.exe is not available; install WSL2 (`wsl --install`) or set PVM_BACKEND=native")
	}
	rewritten := RewriteArgsForWSL(args)
	wslArgs := append([]string{"--", "pvm"}, rewritten...)
	cmd := exec.Command("wsl.exe", wslArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return fatalf("wsl pvm: %v", err)
	}
	return 0
}

// RewriteArgsForWSL converts Windows host paths in `pvm mount` arguments to
// WSL /mnt/<drive>/... paths and restores guest paths rewritten by Git Bash.
func RewriteArgsForWSL(args []string) []string {
	out := append([]string(nil), args...)
	if len(out) == 4 && out[0] == "mount" && out[1] == "rm" {
		out[3] = restoreGitBashPosixPath(out[3], os.Getenv("EXEPATH"))
		return out
	}
	if len(out) < 3 || out[0] != "mount" || out[1] == "ls" || out[1] == "rm" {
		return out
	}
	last := out[len(out)-1]
	host, guest, ok := splitMountSpec(last)
	if !ok || host == "" || guest == "" {
		return out
	}
	if host == "~" || strings.HasPrefix(host, "~/") || strings.HasPrefix(host, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if host == "~" {
				host = home
			} else {
				host = filepath.Join(home, host[2:])
			}
		}
	}
	out[len(out)-1] = WindowsToWSLPath(host) + ":" + guest
	return out
}

func gitBashGuestPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	return restoreGitBashPosixPath(p, os.Getenv("EXEPATH"))
}

func restoreGitBashPosixPath(p, exePath string) string {
	if !hasWindowsDrivePrefix(p) || !hasWindowsDrivePrefix(exePath) {
		return p
	}
	root := strings.TrimRight(strings.ReplaceAll(exePath, `\`, "/"), "/")
	if !strings.HasSuffix(strings.ToLower(root), "/bin") {
		return p
	}
	root = root[:len(root)-4]
	candidate := strings.ReplaceAll(p, `\`, "/")
	if len(candidate) < len(root) || !strings.EqualFold(candidate[:len(root)], root) {
		return p
	}
	rest := candidate[len(root):]
	if rest == "" {
		return "/"
	}
	if !strings.HasPrefix(rest, "/") {
		return p
	}
	return rest
}

// WindowsToWSLPath converts a Windows drive path like `C:\Users\dev\work` or
// `D:/data` to `/mnt/c/Users/dev/work` or `/mnt/d/data`. Paths that are
// already POSIX paths are returned unchanged.
func WindowsToWSLPath(p string) string {
	if !hasWindowsDrivePrefix(p) {
		return p
	}
	drive := strings.ToLower(p[:1])
	rest := strings.ReplaceAll(p[2:], `\`, "/")
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	if rest == "/" {
		return "/mnt/" + drive
	}
	return "/mnt/" + drive + strings.TrimRight(rest, "/")
}

func wslHasPVM() bool {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl.exe", "--", "sh", "-lc", "command -v pvm >/dev/null 2>&1")
	fsutil.ConfigureHiddenCmd(cmd)
	return cmd.Run() == nil
}

// windowsDoctorChecks returns read-only, redacted host readiness checks on Windows.
func windowsDoctorChecks(p *paths.Paths) []auditCheck {
	if runtime.GOOS != "windows" {
		return nil
	}
	var checks []auditCheck
	backend, err := ResolveWindowsBackend(p)
	if err != nil {
		checks = append(checks, auditCheck{"windows_backend", "fail", "invalid backend configuration; use auto, native, or wsl"})
	} else {
		checks = append(checks, auditCheck{"windows_backend", "ok", "effective backend is " + backend})
	}

	if _, err := exec.LookPath("ssh"); err != nil {
		checks = append(checks, auditCheck{"windows_ssh", "warn", "OpenSSH client (ssh.exe) not found in PATH; enable Windows OpenSSH Client"})
	} else {
		checks = append(checks, auditCheck{"windows_ssh", "ok", "OpenSSH client available"})
	}

	tc, tcErr := qemu.Find(p.Runtime)
	wslOK := wslHasPVM()
	switch {
	case tcErr == nil:
		accel := qemu.Detect(tc, "windows")
		if accel == qemu.AccelWHPX {
			checks = append(checks, auditCheck{"windows_accel", "ok", "native QEMU with WHPX hardware acceleration available"})
		} else {
			checks = append(checks, auditCheck{"windows_accel", "warn", "native QEMU found but WHPX is unavailable (using TCG); enable Windows Hypervisor Platform or use PVM_BACKEND=wsl"})
		}
	case wslOK:
		checks = append(checks, auditCheck{"windows_accel", "ok", "WSL2 pvm backend available"})
	default:
		checks = append(checks, auditCheck{"windows_accel", "warn", "neither native QEMU nor WSL2 pvm found; install QEMU or WSL2"})
	}
	return checks
}
