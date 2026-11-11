package qemu

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

type Accel string

const (
	AccelHVF  Accel = "hvf"
	AccelKVM  Accel = "kvm"
	AccelWHPX Accel = "whpx"
	AccelTCG  Accel = "tcg"
)

func ParseAccel(s string) (Accel, bool) {
	switch Accel(s) {
	case AccelHVF, AccelKVM, AccelWHPX, AccelTCG:
		return Accel(s), true
	}
	return "", false
}

func Detect(t *Toolchain, goos string) Accel {
	if env := os.Getenv("PVM_QEMU_ACCEL"); env != "" {
		if a, ok := ParseAccel(env); ok {
			return a
		}
	}
	switch goos {
	case "darwin":
		// The bundled runtime is pvm's own known-good build with a
		// fixed version: trust it supports hvf instead of paying a
		// trial boot on every daemon start. System QEMU installs vary,
		// so those keep the probe.
		if t != nil && t.Bundled {
			return AccelHVF
		}
		if probe(t, "-machine", "virt,accel=hvf") {
			return AccelHVF
		}
	case "linux":
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
			f.Close()
			return AccelKVM
		}
	case "windows":
		if windowsHypervisorPlatformEnabled() && probe(t, "-machine", "q35,accel=whpx,kernel-irqchip=off", "-L", t.FirmwareDir) {
			return AccelWHPX
		}
	}
	return AccelTCG
}

// probe starts qemu briefly with the given machine args; if it is still
// alive after a short delay the accelerator works.
func probe(t *Toolchain, args ...string) bool {
	if t == nil || t.System == "" {
		return false
	}
	args = append(args, "-display", "none", "-S")
	cmd := exec.Command(t.System, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	fsutil.ConfigureHiddenCmd(cmd)
	if err := cmd.Start(); err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return false
	case <-time.After(300 * time.Millisecond):
		cmd.Process.Kill()
		<-done
		return true
	}
}

func (a Accel) Args() []string {
	switch a {
	case AccelTCG:
		return []string{"-accel", "tcg,thread=multi", "-cpu", "max"}
	case AccelWHPX:
		// The bundled QEMU 11.1 WHPX backend cannot sync XSAVE state with
		// Hyper-V: exposing XSAVE corrupts FPU state when secondary vCPUs
		// come up, and kernel-irqchip=on triple-faults at boot. Hiding
		// XSAVE alone leaves Hyper-V's CET bits visible, so the guest also
		// needs GuestKernelArgs. vmport reads force full register syncs,
		// and guest-initiated resets hang, so exit instead of rebooting.
		return []string{"-accel", "whpx,kernel-irqchip=off", "-machine", "vmport=off", "-cpu", "host,-xsave", "-no-reboot"}
	default: // AccelHVF, AccelKVM
		return []string{"-accel", string(a), "-cpu", "host"}
	}
}

// ArgsForGuest keeps a WHPX guest provisioned without noxsave on qemu64:
// its kernel may enable CET state that the host CPU model cannot switch safely.
func (a Accel) ArgsForGuest(kernelArgs string) []string {
	if a == AccelWHPX && !slices.Contains(strings.Fields(kernelArgs), "noxsave") {
		return []string{"-accel", "whpx,kernel-irqchip=off", "-machine", "vmport=off", "-cpu", "qemu64", "-no-reboot"}
	}
	return a.Args()
}

// GuestKernelArgs are kernel parameters the guest must boot with under
// this accelerator. Under WHPX, noxsave keeps the guest kernel from
// enabling CET shadow stacks, whose per-task state it can only switch
// with XSAVES.
func (a Accel) GuestKernelArgs() string {
	if a == AccelWHPX {
		return "noxsave"
	}
	return ""
}

func (a Accel) Fallback() (Accel, bool) {
	if a != AccelTCG {
		return AccelTCG, true
	}
	return a, false
}

// HV_NO_DEVICE means macOS Hypervisor.framework refuses new VMs (typically
// after long uptime / sleep cycles) until the host reboots.
func FallbackHint(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "HV_NO_DEVICE") {
		return " (macOS Hypervisor.framework is refusing new VMs; reboot the host to restore hardware acceleration)"
	}
	if strings.Contains(msg, "WHPX:") || strings.Contains(msg, "Windows Hypervisor Platform") {
		return " (Windows Hypervisor Platform is unavailable; enable it in Windows Features and reboot to restore hardware acceleration)"
	}
	return ""
}
