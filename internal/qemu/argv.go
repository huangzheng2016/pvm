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
	// Incoming, when set, appends `-incoming <value>` (RAM snapshot
	// restore, e.g. "exec:/path/pvm __snapshot-pipe-in <file>").
	Incoming string
	// NoVirtFSDevices emits only the 9p backends/root-ports, leaving
	// the front-end devices out; used on restore, where the devices
	// are hot-added via QMP device_add after the migration completes.
	NoVirtFSDevices bool
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
	if c.Incoming != "" {
		args = append(args, "-incoming", c.Incoming)
	}

	fs := []Mount{{Tag: "pvmshare", HostPath: c.ShareDir}}
	fs = append(fs, c.ExtraMounts...)
	for i, m := range fs {
		id := fmt.Sprintf("fs%d", i)
		// security_model=none: server does all host I/O as the qemu
		// process user; guest-side enforcement is disabled by mounting
		// with access=any (see cloud-init). mapped-xattr was tried and
		// rejects even root readdir on macOS hosts.
		// id=<fsN> lets the snapshot flow hot-unplug the device via
		// QMP device_del (9p otherwise blocks migration). pcie.0
		// itself does not support hotplug, so each share sits behind
		// a pcie-root-port on a high slot (dodging auto-assignment).
		args = append(args,
			"-device", fmt.Sprintf("pcie-root-port,id=rp%d,bus=pcie.0,addr=0x%x", i, 0x8+i),
			"-fsdev", fmt.Sprintf("local,id=%s,path=%s,security_model=none", id, m.HostPath),
		)
		if !c.NoVirtFSDevices {
			args = append(args,
				"-device", fmt.Sprintf("virtio-9p-pci-non-transitional,id=%s,fsdev=%s,mount_tag=%s,bus=rp%d", id, id, m.Tag, i),
			)
		}
	}
	return args, nil
}
