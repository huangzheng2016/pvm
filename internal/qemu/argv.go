package qemu

import (
	"fmt"
	"golang.org/x/sys/unix"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/huangzheng2016/pvm/internal/native"
)

const MaxExtraMounts = 13

type Mount struct {
	Tag      string
	HostPath string
}

// VirtFSSlot returns the stable PCI slot and fsdev number for a mount tag.
func VirtFSSlot(tag string) (int, error) {
	if tag == "pvmshare" {
		return 0, nil
	}
	if !strings.HasPrefix(tag, "pvm") {
		return 0, fmt.Errorf("invalid mount tag %q", tag)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(tag, "pvm"))
	if err != nil || n < 0 || n >= MaxExtraMounts {
		return 0, fmt.Errorf("invalid mount tag %q", tag)
	}
	return n + 1, nil
}

func VirtFSID(slot int) string       { return fmt.Sprintf("fs%d", slot) }
func VirtFSRootPort(slot int) string { return fmt.Sprintf("rp%d", slot) }

type NetMode string

const (
	// NetSlirp is QEMU user-mode networking with hostfwd (gateway VMs
	// and standalone VMs such as image bake).
	NetSlirp NetMode = "slirp"
	// NetCluster attaches the only NIC to a cluster L2 segment via a
	// point-to-point stream socket to the gateway VM.
	NetCluster NetMode = "cluster"
)

type Network struct {
	Mode        NetMode
	SSHHostPort int
	PoolBase    int // gateway only: hostfwd pool start (tcp+udp)
	PoolSize    int
	LinkSock    string // unix socket the gateway listens on
	MAC         string
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
	MountDir     string // daemon-controlled symlinks for dynamic mount backends
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
	GPU         bool // virtio-gpu display device
	Balloon     bool // virtio-balloon with free-page-reporting
}

func validateUnixSocketPath(name, path string) error {
	if len(path) >= len(unix.RawSockaddrUnix{}.Path) {
		return fmt.Errorf("%s Unix socket path is %d bytes (maximum %d); use a shorter PVM_HOME", name, len(path), len(unix.RawSockaddrUnix{}.Path)-1)
	}
	return nil
}

func keyval(s string) string { return strings.ReplaceAll(s, ",", ",,") }

// BuildArgs constructs the full qemu argv (excluding the binary itself).
func BuildArgs(t *Toolchain, c *VMConfig) ([]string, error) {
	for _, socket := range []struct{ name, path string }{{"QMP", c.QMPSock}, {"serial", c.SerialSock}, {"agent", c.AgentSock}, {"network link", c.Network.LinkSock}} {
		if err := validateUnixSocketPath(socket.name, socket.path); err != nil {
			return nil, err
		}
	}
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
			"-drive", "if=pflash,format=raw,readonly=on,file="+keyval(code),
			"-drive", "if=pflash,format=raw,file="+keyval(c.VarsPath),
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
		"-chardev", fmt.Sprintf("socket,id=ser0,path=%s,server=on,wait=off,logfile=%s", keyval(c.SerialSock), keyval(c.ConsoleLog)),
		"-serial", "chardev:ser0",
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio,cache=writeback,discard=unmap,detect-zeroes=unmap", keyval(c.DiskPath)),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", keyval(c.SeedPath)),
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-pci,rng=rng0",
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", keyval(c.QMPSock)),
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
			"-chardev", fmt.Sprintf("socket,id=pvmagent,path=%s,server=on,wait=off", keyval(c.AgentSock)),
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
	if c.MountDir != "" {
		fs = make([]Mount, 1+MaxExtraMounts)
		fs[0] = Mount{Tag: "pvmshare", HostPath: c.ShareDir}
		for slot := 1; slot <= MaxExtraMounts; slot++ {
			fs[slot].HostPath = filepath.Join(c.MountDir, fmt.Sprintf("slot%d", slot))
		}
	}
	for _, m := range c.ExtraMounts {
		if c.MountDir == "" {
			return nil, fmt.Errorf("mount %s needs a dynamic mount directory", m.Tag)
		}
		slot, err := VirtFSSlot(m.Tag)
		if err != nil || slot == 0 {
			return nil, fmt.Errorf("invalid extra mount tag %q", m.Tag)
		}
		if fs[slot].Tag != "" {
			return nil, fmt.Errorf("duplicate mount tag %q", m.Tag)
		}
		if m.HostPath == "" {
			return nil, fmt.Errorf("mount %s has an empty backend path", m.Tag)
		}
		fs[slot] = m
	}
	for i, m := range fs {
		id := VirtFSID(i)
		rp := VirtFSRootPort(i)
		// security_model=none: server does all host I/O as the qemu
		// process user; guest-side enforcement is disabled by mounting
		// with access=any (see cloud-init). mapped-xattr was tried and
		// rejects even root readdir on macOS hosts.
		// Inactive slots keep only their backend and root port. The daemon
		// retargets the backend symlink before hot-adding the front-end.
		args = append(args,
			"-device", fmt.Sprintf("pcie-root-port,id=%s,bus=pcie.0,addr=0x%x,chassis=%d", rp, 0x10+i, 128+i),
			"-fsdev", fmt.Sprintf("local,id=%s,path=%s,security_model=none", id, keyval(m.HostPath)),
		)
		if m.Tag != "" && !c.NoVirtFSDevices {
			args = append(args,
				"-device", fmt.Sprintf("virtio-9p-pci-non-transitional,id=%s,fsdev=%s,mount_tag=%s,bus=%s", id, id, m.Tag, rp),
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
			"-netdev", fmt.Sprintf("stream,id=net0,server=off,addr.type=unix,addr.path=%s", keyval(n.LinkSock)),
			"-device", fmt.Sprintf("virtio-net-pci,id=nic0,netdev=net0,mac=%s,bus=rp-link", keyval(n.MAC)),
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
