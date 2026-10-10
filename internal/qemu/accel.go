package qemu

import (
	"os"
	"os/exec"
	"time"
)

type Accel string

const (
	AccelHVF Accel = "hvf"
	AccelKVM Accel = "kvm"
	AccelTCG Accel = "tcg"
)

// ParseAccel validates a user-supplied accelerator name.
func ParseAccel(s string) (Accel, bool) {
	switch Accel(s) {
	case AccelHVF, AccelKVM, AccelTCG:
		return Accel(s), true
	}
	return "", false
}

// Detect picks the best accelerator: PVM_QEMU_ACCEL env override first,
// then hvf probe on darwin, /dev/kvm on linux, TCG fallback.
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
		if probe(t, "-machine", "virt,accel=hvf") || probe(t, "-machine", "q35,accel=hvf") {
			return AccelHVF
		}
	case "linux":
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
			f.Close()
			return AccelKVM
		}
	}
	return AccelTCG
}

// probe starts qemu briefly with the given machine args; if it is still
// alive after a short delay the accelerator works.
func probe(t *Toolchain, args ...string) bool {
	if t == nil {
		return false
	}
	args = append(args, "-display", "none", "-S")
	cmd := exec.Command(t.System, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
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

// Args returns the accel/cpu argv fragments for this accelerator.
func (a Accel) Args() []string {
	if a == AccelTCG {
		return []string{"-accel", "tcg,thread=multi", "-cpu", "max"}
	}
	return []string{"-accel", string(a), "-cpu", "host"}
}

// Fallback returns the accelerator to retry with after an early crash.
func (a Accel) Fallback() (Accel, bool) {
	if a != AccelTCG {
		return AccelTCG, true
	}
	return a, false
}
