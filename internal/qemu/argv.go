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

// NetMode selects the guest network topology.
type NetMode string

const (
	// NetSlirp is QEMU user-mode networking with hostfwd (gateway VMs
	// and pre-M2 standalone VMs).
	NetSlirp NetMode = "slirp"
	// NetCluster attaches the only NIC to a cluster L2 segment via a
	// point-to-point stream socket to the gateway VM.
	NetCluster NetMode = "cluster"
)

// Network is the per-VM network configuration.
type Network struct {
	Mode NetMode
	// slirp mode
	SSHHostPort int
	PoolBase    int // gateway only: hostfwd pool start (tcp+udp)
	PoolSize    int
	// cluster mode
	LinkSock string // unix socket the gateway listens on
	MAC      string
}

type VMConfig struct {
	Name       string
	Arch       string // GOARCH: arm64 / amd64
	CPUs       int
	MemMB      int
	DiskPath   string
	SeedPath   string
	VarsPath   string // per-VM UEFI vars (aarch64)
	ConsoleLog string
	QMPSock    string
	SerialSock string
	Network    Network
	// HotplugPorts pre-creates pcie-root-ports rp10..rp(9+N) so NICs can
	// be hot-added later (pcie.0 itself does not support hotplug).
	// Gateway VMs get one port per possible cluster VM IP octet.
	HotplugPorts int
	ShareDir     string // default ~/.pvm/share mounted at /share
	ExtraMounts  []Mount
	Accel        Accel
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
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-pci,rng=rng0",
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", c.QMPSock),
	)

	args = append(args, netArgs(c.Network)...)
	for i := 0; i < c.HotplugPorts; i++ {
		// root ports need explicit chassis+addr; slots 8-15 x 8
		// functions = 64 ports, clear of the auto-assigned low slots
		// used by our boot devices. fn0 must declare multifunction so
		// functions 1-7 may share the slot.
		mf := ""
		if i%8 == 0 {
			mf = ",multifunction=on"
		}
		args = append(args, "-device", fmt.Sprintf("pcie-root-port,id=rp%d,bus=pcie.0,addr=0x%x.%d,chassis=%d%s", 10+i, 8+i/8, i%8, i+1, mf))
	}

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

// netArgs renders the -netdev/-device pair for the configured mode.
func netArgs(n Network) []string {
	switch n.Mode {
	case NetCluster:
		// the NIC sits on its own root port so the daemon can
		// hot-replace it (device_del/netdev_add/device_add) after a
		// gateway reboot: stream netdevs have no client reconnect.
		// device id nic0, netdev id net0: QMP object ids share one
		// namespace, so they must differ.
		return []string{
			"-device", "pcie-root-port,id=rp-link,bus=pcie.0,addr=0x8.0,chassis=1,multifunction=on",
			"-netdev", fmt.Sprintf("stream,id=net0,server=off,addr.type=unix,addr.path=%s", n.LinkSock),
			"-device", fmt.Sprintf("virtio-net-pci,id=nic0,netdev=net0,mac=%s,bus=rp-link", n.MAC),
		}
	default: // NetSlirp
		fwd := fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:22", n.SSHHostPort)
		if n.PoolBase > 0 && n.PoolSize > 0 {
			for p := n.PoolBase; p < n.PoolBase+n.PoolSize; p++ {
				fwd += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:%d", p, p)
				fwd += fmt.Sprintf(",hostfwd=udp:127.0.0.1:%d-:%d", p, p)
			}
		}
		return []string{
			"-netdev", "user,id=net0," + fwd,
			"-device", "virtio-net-pci,netdev=net0",
		}
	}
}
