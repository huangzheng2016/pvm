package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type BakeOptions struct {
	Spec      string // e.g. ubuntu:24.04
	P         *paths.Paths
	TC        *qemu.Toolchain
	Accel     qemu.Accel
	SSHKey    string
	SSHPub    string
	Out       io.Writer // progress log
	KeepVM    bool      // keep the build VM for debugging
	BuildCPUs int
	BuildMem  int
	BuildDisk int
}

type sshRunner struct {
	key  string
	port int
	user string
}

func (r *sshRunner) args(extra ...string) []string {
	base := []string{
		"-i", r.key,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-p", strconv.Itoa(r.port),
		fmt.Sprintf("%s@127.0.0.1", r.user),
	}
	return append(base, extra...)
}

// runScript streams a script to `sudo bash -s` in the guest.
func (r *sshRunner) runScript(script string, out io.Writer) error {
	cmd := exec.Command("ssh", r.args("sudo", "bash", "-s")...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func (r *sshRunner) output(args ...string) (string, error) {
	cmd := exec.Command("ssh", r.args(args...)...)
	b, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(b)), err
}

// Bake builds a golden image from a fetched base image.
func Bake(o *BakeOptions) (*GoldenEntry, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	ref, err := Resolve(o.Spec, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(o.P.Images, ref.FileName())
	if _, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("base image %s not found; run `pvm image fetch %s`", base, o.Spec)
	}
	stem := fmt.Sprintf("%s-%s", ref.Distro, ref.Version)
	// vm names reject dots, so the build-vm stem flattens them away.
	nameStem := strings.ReplaceAll(stem, ".", "")

	// Idempotency: drop stale build VMs from interrupted runs.
	if specs, err := vm.ListAll(o.P); err == nil {
		for _, s := range specs {
			if strings.HasPrefix(s.Name, "bake-"+nameStem+"-") {
				fmt.Fprintf(o.Out, "cleaning stale build vm %s\n", s.Name)
				inst := qemu.Instance{Cfg: s.QEMUConfig(o.P, o.Accel),
					StatePath: filepath.Join(o.P.VMDir(s.Name), "run.json"), Toolchain: o.TC}
				inst.Stop()
				vm.Delete(o.P, s.Name)
			}
		}
	}

	cpus, mem, disk := o.BuildCPUs, o.BuildMem, o.BuildDisk
	if cpus == 0 {
		cpus = 4
	}
	if mem == 0 {
		mem = 6144
	}
	if disk == 0 {
		disk = 25
	}
	port, err := vm.AllocSSHPort(vm.UsedSSHPorts(o.P), vm.SSHPortBase)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("bake-%s-%d", nameStem, time.Now().Unix())
	spec := &vm.Spec{
		Name: name, Image: o.Spec, User: "baker", Arch: runtime.GOARCH,
		GuestUID: os.Getuid(),
		CPUs:     cpus, MemMB: mem, DiskGB: disk,
		SSHPort: port, BaseDisk: base,
	}
	fmt.Fprintf(o.Out, "creating build vm %s (base %s)\n", name, ref.FileName())
	if err := vm.Create(o.P, o.TC, spec, o.SSHPub); err != nil {
		return nil, err
	}
	defer func() {
		if !o.KeepVM {
			vm.Delete(o.P, name)
		}
	}()

	inst := &qemu.Instance{
		Cfg:       spec.QEMUConfig(o.P, o.Accel),
		StatePath: filepath.Join(o.P.VMDir(name), "run.json"),
		Toolchain: o.TC,
	}
	if err := inst.Start(); err != nil {
		return nil, fmt.Errorf("start build vm: %w", err)
	}
	defer func() {
		if inst.Running() != nil {
			inst.Stop()
		}
	}()

	r := &sshRunner{key: o.SSHKey, port: port, user: spec.User}
	fmt.Fprintf(o.Out, "waiting for ssh (cloud-init first boot)...\n")
	if err := waitSSH(r, 10*time.Minute); err != nil {
		return nil, err
	}

	prov, err := renderProvision(ref.Distro, spec.User)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(o.Out, "provisioning (apt upgrade, docker, kasm pull — this takes a while)...\n")
	// The provision script deletes the build user as its last action, so
	// nothing may ssh in afterwards; capture the image list from the log.
	var provLog strings.Builder
	if err := r.runScript(prov, io.MultiWriter(o.Out, &provLog)); err != nil {
		return nil, fmt.Errorf("provision: %w", err)
	}
	dockerImages := parsePreinstalled(provLog.String())
	fmt.Fprintf(o.Out, "preinstalled docker images: %v\n", dockerImages)

	// Graceful ACPI powerdown via QMP; ssh is gone with the build user.
	fmt.Fprintf(o.Out, "shutting down build vm (QMP powerdown)...\n")
	if err := inst.Stop(); err != nil {
		return nil, fmt.Errorf("stop build vm: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for inst.Running() != nil && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if inst.Running() != nil {
		return nil, fmt.Errorf("build vm did not power off")
	}

	digest, err := fileSHA256(base)
	if err != nil {
		return nil, err
	}
	cat, err := loadGoldenCatalog(o.P.Images)
	if err != nil {
		return nil, err
	}
	version := 1
	if ms := cat.match(ref.Distro, ref.Version, runtime.GOARCH); len(ms) > 0 {
		version = ms[0].Version + 1
	}
	entry := GoldenEntry{
		Version: version, Distro: ref.Distro, ImageVersion: ref.Version,
		Arch: runtime.GOARCH, File: goldenFileName(ref.Distro, ref.Version, version),
		BaseDigest: digest, BuiltAt: time.Now(),
		Packages: GoldenPackages, DockerImages: dockerImages,
	}
	if err := os.MkdirAll(GoldenDir(o.P.Images), 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(GoldenDir(o.P.Images), entry.File)
	tmp := dst + ".tmp"
	fmt.Fprintf(o.Out, "flattening %s -> %s\n", spec.Disk(o.P), dst)
	out, err := exec.Command(o.TC.Img, "convert", "-O", "qcow2", spec.Disk(o.P), tmp).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	cat.Entries = append(cat.Entries, entry)
	if err := cat.save(o.P.Images); err != nil {
		return nil, err
	}
	fmt.Fprintf(o.Out, "golden image ready: %s v%d\n", o.Spec, version)
	return &entry, nil
}

func waitSSH(r *sshRunner, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		cmd := exec.Command("ssh", r.args("true")...)
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("ssh not ready after %s: %v", timeout, last)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
