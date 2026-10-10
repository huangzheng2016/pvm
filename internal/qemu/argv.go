package qemu

import (
	"fmt"
	"path/filepath"
	"strconv"
)

type Mount struct {
	Tag      string
	HostPath string
}

type VMConfig struct {
	Name        string
	Arch        string // GOARCH: arm64 / amd64
	CPUs        int
	MemMB       int
	DiskPath    string
	SeedPath    string
	VarsPath    string // per-VM UEFI vars (aarch64)
	ConsoleLog  string
	QMPSock     string
	SerialSock  string
	SSHHostPort int
	ShareDir    string // default ~/.pvm/share mounted at /share
	ExtraMounts []Mount
	Accel       Accel
}

// BuildArgs constructs the full qemu argv (excluding the binary itself).
func BuildArgs(t *Toolchain, c *VMConfig) ([]string, error) {
	if c.CPUs <= 0 {
		c.CPUs = 2
	}
	if c.MemMB <= 0 {
		c.MemMB = 4096
	}
	args := []string{}

	if c.Arch == "arm64" {
		args = append(args, "-machine", "virt")
		if t.FirmwareDir == "" {
			return nil, fmt.Errorf("aarch64 VMs need UEFI firmware (edk2-aarch64-code.fd); firmware dir unknown")
		}
		code := filepath.Join(t.FirmwareDir, "edk2-aarch64-code.fd")
		args = append(args,
			"-drive", "if=pflash,format=raw,readonly=on,file="+code,
			"-drive", "if=pflash,format=raw,file="+c.VarsPath,
		)
	} else {
		// SeaBIOS default; amd64 cloud images are hybrid and boot fine.
		args = append(args, "-machine", "q35")
	}

	args = append(args, c.Accel.Args()...)
	args = append(args,
		"-m", strconv.Itoa(c.MemMB),
		"-smp", strconv.Itoa(c.CPUs),
		"-display", "none",
		"-chardev", fmt.Sprintf("socket,id=ser0,path=%s,server=on,wait=off,logfile=%s", c.SerialSock, c.ConsoleLog),
		"-serial", "chardev:ser0",
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio,cache=writeback,discard=unmap,detect-zeroes=unmap", c.DiskPath),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", c.SeedPath),
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", c.SSHHostPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-pci,rng=rng0",
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", c.QMPSock),
	)

	fs := []Mount{{Tag: "pvmshare", HostPath: c.ShareDir}}
	fs = append(fs, c.ExtraMounts...)
	for i, m := range fs {
		id := fmt.Sprintf("fs%d", i)
		// security_model=none: server does all host I/O as the qemu
		// process user; guest-side enforcement is disabled by mounting
		// with access=any (see cloud-init). mapped-xattr was tried and
		// rejects even root readdir on macOS hosts.
		args = append(args,
			"-fsdev", fmt.Sprintf("local,id=%s,path=%s,security_model=none", id, m.HostPath),
			"-device", fmt.Sprintf("virtio-9p-pci,fsdev=%s,mount_tag=%s", id, m.Tag),
		)
	}
	return args, nil
}
