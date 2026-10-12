package qemu

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/huangzheng2016/pvm/internal/native"
)

type Mount struct {
	Tag      string
	HostPath string
}

// NetMode selects the guest network topology.
type NetMode string

const (
	// NetSlirp is QEMU user-mode networking with hostfwd (gateway VMs
	// and standalone VMs such as image bake).
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
	CPUs       int
	MemMB      int
	DiskPath   string
	SeedPath   string
	VarsPath   string // per-VM UEFI vars (aarch64)
	ConsoleLog string
	QMPSock    string
	SerialSock string
	AgentSock  string // unix socket for the virtio-serial agent channel; empty disables
	Network    Network
	// HotplugPorts pre-creates pcie-root-ports rp10..rp(9+N) so NICs can
	// be hot-added later (pcie.0 itself does not support hotplug).
	// Gateway VMs get one port per possible cluster VM IP octet.
	HotplugPorts int
	ShareDir     string // default ~/.pvm/share mounted at /share
	ExtraMounts  []Mount
	Accel        Accel
	// Incoming, when set, appends `-incoming <value>` (RAM snapshot
	// restore, e.g. "exec:/path/pvm __snapshot-pipe-in <file>").
	Incoming string
	// NoVirtFSDevices emits only the 9p backends/root-ports, leaving
	// the front-end devices out; used on restore, where the devices
	// are hot-added via QMP device_add after the migration completes.
	NoVirtFSDevices bool
	// SmbiosSpoof presents common commodity hardware in DMI instead of
	// the QEMU defaults.
	SmbiosSpoof bool
	// GPU attaches a virtio-gpu display device (guest gets /dev/dri);
	// headless VMs (gateway, image bake) leave it off.
	GPU bool
	// Balloon attaches virtio-balloon with free-page-reporting: the guest
	// continuously returns idle pages to the host, which is what makes
	// ram overcommit across vms safe. User VMs default it on.
	Balloon bool
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

	if native.Arch == "arm64" {
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
	if c.SmbiosSpoof {
		// A coherent desktop persona: VMs have no battery/ACPI power
		// supply, so a laptop model would contradict itself.
		args = append(args,
			"-smbios", "type=0,vendor=Dell Inc.,version=2.7.0,date=03/16/2023",
			"-smbios", "type=1,manufacturer=Dell Inc.,product=OptiPlex 7010,version=01",
			"-smbios", "type=2,manufacturer=Dell Inc.,product=0GY6YJ,version=A01",
			"-smbios", "type=3,manufacturer=Dell Inc.,version=01",
		)
	}
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
	if c.GPU {
		args = append(args, "-device", "virtio-gpu-pci")
	}
	if c.Balloon {
		args = append(args, "-device", "virtio-balloon-pci,free-page-reporting=on")
	}
	if c.Incoming != "" {
		args = append(args, "-incoming", c.Incoming)
	}

	if c.AgentSock != "" {
		args = append(args,
			"-device", "virtio-serial-pci",
			"-chardev", fmt.Sprintf("socket,id=pvmagent,path=%s,server=on,wait=off", c.AgentSock),
			"-device", "virtserialport,chardev=pvmagent,name=pvm.agent",
		)
	}

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
		// id=<fsN> lets the snapshot flow hot-unplug the device via
		// QMP device_del (9p otherwise blocks migration). pcie.0
		// itself does not support hotplug, so each share sits behind
		// a pcie-root-port on its own slot (0x10+i), clear of the
		// hotplug range (0x8-0xf) and rp-link (0x1e).
		args = append(args,
			"-device", fmt.Sprintf("pcie-root-port,id=rp%d,bus=pcie.0,addr=0x%x,chassis=%d", i, 0x10+i, 128+i),
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

// netArgs renders the -netdev/-device pair for the configured mode.
func netArgs(n Network) []string {
	switch n.Mode {
	case NetCluster:
		// the NIC sits on its own root port so the daemon can
		// hot-replace it (device_del/netdev_add/device_add) after a
		// gateway reboot: stream netdevs have no client reconnect.
		// device id nic0, netdev id net0: QMP object ids share one
		// namespace, so they must differ. slot 0x1e keeps clear of the
		// NIC hotplug range (0x8-0xf) and the 9p share ports (0x10+).
		return []string{
			"-device", "pcie-root-port,id=rp-link,bus=pcie.0,addr=0x1e.0,chassis=96",
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
