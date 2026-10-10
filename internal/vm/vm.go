// Package vm orchestrates VM specs: overlay disks, cloud-init seeds,
// port allocation and on-disk metadata. Process control lives in
// internal/qemu; this package owns what a VM *is*.
package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

type Spec struct {
	Name  string `json:"name"`
	Image string `json:"image"` // e.g. ubuntu:24.04
	Role  string `json:"role"`  // user | gateway
	User  string `json:"user"`
	// GuestUID pins the guest user's uid to the host user's uid so 9p
	// shares behave sanely (same trick as lima/docker-desktop).
	GuestUID int    `json:"guest_uid"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	MemMB    int    `json:"mem_mb"`
	DiskGB   int    `json:"disk_gb"`
	SSHPort  int    `json:"ssh_port"` // slirp hostfwd (gateways only)
	BaseDisk string `json:"base_disk"`
	// Golden is the golden image version the VM was cloned from
	// (0 = plain base image). Golden clones skip first-boot apt.
	Golden int `json:"golden,omitempty"`

	// cluster membership (user VMs)
	ClusterID int    `json:"cluster_id,omitempty"`
	IP        string `json:"ip,omitempty"`
	MAC       string `json:"mac,omitempty"`

	CreatedAt time.Time    `json:"created_at"`
	Mounts    []GuestMount `json:"mounts,omitempty"`

	// Region/identity profile. TZ/Locale are applied by cloud-init at
	// first boot; PrevTZ/PrevLocale remember the pre-route values so
	// `route --via direct` can restore them.
	TZ          string `json:"tz,omitempty"`
	Locale      string `json:"locale,omitempty"`
	GitEmail    string `json:"git_email,omitempty"`
	GitName     string `json:"git_name,omitempty"`
	SmbiosSpoof bool   `json:"smbios_spoof,omitempty"`
	PrevTZ      string `json:"prev_tz,omitempty"`
	PrevLocale  string `json:"prev_locale,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`)

func (s *Spec) dir(p *paths.Paths) string  { return p.VMDir(s.Name) }
func (s *Spec) Disk(p *paths.Paths) string { return filepath.Join(s.dir(p), "disk.qcow2") }
func (s *Spec) Seed(p *paths.Paths) string { return filepath.Join(s.dir(p), "seed.img") }
func (s *Spec) Vars(p *paths.Paths) string { return filepath.Join(s.dir(p), "vars.fd") }
func (s *Spec) SpecPath(p *paths.Paths) string {
	return filepath.Join(s.dir(p), "vm.json")
}

// Load reads a VM spec by name.
func Load(p *paths.Paths, name string) (*Spec, error) {
	data, err := os.ReadFile(p.VMs + "/" + name + "/vm.json")
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListAll returns specs for every VM directory with a vm.json.
func ListAll(p *paths.Paths) ([]*Spec, error) {
	entries, err := os.ReadDir(p.VMs)
	if err != nil {
		return nil, err
	}
	var out []*Spec
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := Load(p, e.Name())
		if err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func (s *Spec) Save(p *paths.Paths) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.SpecPath(p), data, 0o600)
}

// QEMUConfig derives the qemu runtime config from the spec.
func (s *Spec) QEMUConfig(p *paths.Paths, accel qemu.Accel, linkSock string, poolBase, poolSize int) qemu.VMConfig {
	dir := s.dir(p)
	mounts := make([]qemu.Mount, 0, len(s.Mounts))
	for _, m := range s.Mounts {
		mounts = append(mounts, qemu.Mount{Tag: m.Tag, HostPath: m.Host})
	}
	var net qemu.Network
	hotplug := 0
	switch {
	case s.Role == "gateway":
		net = qemu.Network{Mode: qemu.NetSlirp, SSHHostPort: s.SSHPort, PoolBase: poolBase, PoolSize: poolSize}
		hotplug = 64 // rp10..rp73, one per cluster VM slot
	case linkSock != "":
		net = qemu.Network{Mode: qemu.NetCluster, LinkSock: linkSock, MAC: s.MAC}
	default:
		// no cluster attachment (image bake and other standalone VMs)
		net = qemu.Network{Mode: qemu.NetSlirp, SSHHostPort: s.SSHPort}
	}
	return qemu.VMConfig{
		Name:         s.Name,
		Arch:         s.Arch,
		CPUs:         s.CPUs,
		MemMB:        s.MemMB,
		DiskPath:     s.Disk(p),
		SeedPath:     s.Seed(p),
		VarsPath:     s.Vars(p),
		ConsoleLog:   filepath.Join(dir, "console.log"),
		QMPSock:      filepath.Join(dir, "qmp.sock"),
		SerialSock:   filepath.Join(dir, "serial.sock"),
		AgentSock:    p.AgentSock(s.Name),
		Network:      net,
		HotplugPorts: hotplug,
		ShareDir:     p.Share,
		ExtraMounts:  mounts,
		Accel:        accel,
		SmbiosSpoof:  s.SmbiosSpoof,
	}
}

// Create builds the VM directory: overlay disk, UEFI vars, seed ISO,
// spec file. It does not start the VM.
func Create(p *paths.Paths, tc *qemu.Toolchain, spec *Spec, sshPub string) error {
	if !nameRe.MatchString(spec.Name) {
		return fmt.Errorf("invalid vm name %q (letters/digits/-_, <=32, must start with a letter)", spec.Name)
	}
	if _, err := os.Stat(spec.BaseDisk); err != nil {
		return fmt.Errorf("base image %s not found; run `pvm image fetch %s`", spec.BaseDisk, spec.Image)
	}
	dir := spec.dir(p)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("vm %q already exists", spec.Name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	overlay := spec.Disk(p)
	out, err := exec.Command(tc.Img, "create", "-f", "qcow2", "-b", spec.BaseDisk, "-F", "qcow2", overlay).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create: %v\n%s", err, out)
	}
	if spec.DiskGB > 0 {
		// Never shrink: a golden base may already be larger than the
		// requested size (e.g. baked from a 25G build disk).
		cur, err := virtualSizeGB(tc, overlay)
		if err != nil {
			return err
		}
		if spec.DiskGB > cur {
			out, err := exec.Command(tc.Img, "resize", overlay, fmt.Sprintf("%dG", spec.DiskGB)).CombinedOutput()
			if err != nil {
				return fmt.Errorf("qemu-img resize: %v\n%s", err, out)
			}
		}
	}

	if spec.Arch == "arm64" {
		// 64 MiB blank pflash; OVMF formats it on first boot.
		if err := os.WriteFile(spec.Vars(p), make([]byte, 64*1024*1024), 0o600); err != nil {
			return err
		}
	}

	ud, err := userData(spec, sshPub, spec.Mounts)
	if spec.Role == "gateway" {
		ud, err = gatewayUserData(spec, sshPub)
	}
	if err != nil {
		return err
	}
	if err := WriteSeed(spec.Seed(p), ud, metaData(spec)); err != nil {
		return fmt.Errorf("seed image: %w", err)
	}

	spec.CreatedAt = time.Now()
	return spec.Save(p)
}

// Delete removes the VM directory. Caller must stop the VM first.
func Delete(p *paths.Paths, name string) error {
	return os.RemoveAll(p.VMDir(name))
}

// UsedSSHPorts returns the ports already assigned to existing VMs.
func UsedSSHPorts(p *paths.Paths) map[int]bool {
	used := map[int]bool{}
	specs, err := ListAll(p)
	if err != nil {
		return used
	}
	for _, s := range specs {
		if s.SSHPort > 0 {
			used[s.SSHPort] = true
		}
	}
	return used
}
