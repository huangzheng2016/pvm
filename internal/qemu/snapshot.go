package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

// RAM snapshot layout inside the VM directory:
//   state.snapshot.zst  RAM + device state, zstd compressed
//   disk.snap.qcow2     flattened standalone base captured at snapshot time
//   disk.restore.qcow2  per-restore overlay backed by disk.snap.qcow2
//   snapshot.json       manifest
//
// Two platform bugs are deliberately not repeated:
//  1. any failure after QMP stop resumes the guest with cont (defer);
//  2. the old snap base is only replaced after the new one is fully
//     converted (temp file + rename), never deleted beforehand.
type SnapshotManifest struct {
	Name       string    `json:"name"`
	Arch       string    `json:"arch"`
	CPUs       int       `json:"cpus"`
	MemMB      int       `json:"mem_mb"`
	Accel      Accel     `json:"accel"`
	PVMVersion string    `json:"pvm_version"`
	CreatedAt  time.Time `json:"created_at"`
	Consumed   bool      `json:"consumed"`
}

var ErrNoSnapshot = errors.New("no snapshot")

type SnapshotPaths struct {
	Dir      string
	RAM      string
	Snap     string
	Restore  string
	Manifest string
}

func SnapPaths(vmDir string) SnapshotPaths {
	return SnapshotPaths{
		Dir:      vmDir,
		RAM:      filepath.Join(vmDir, "state.snapshot.zst"),
		Snap:     filepath.Join(vmDir, "disk.snap.qcow2"),
		Restore:  filepath.Join(vmDir, "disk.restore.qcow2"),
		Manifest: filepath.Join(vmDir, "snapshot.json"),
	}
}

func LoadManifest(vmDir string) (*SnapshotManifest, error) {
	data, err := os.ReadFile(SnapPaths(vmDir).Manifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	var m SnapshotManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("snapshot manifest: %w", err)
	}
	return &m, nil
}

func (m *SnapshotManifest) save(vmDir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(SnapPaths(vmDir).Manifest, data, 0o600)
}

// checkMatch verifies a manifest against the VM config it would restore
// into; a heterogeneous restore is refused outright.
func (m *SnapshotManifest) checkMatch(c *VMConfig) error {
	if m.Arch != c.Arch || m.CPUs != c.CPUs || m.MemMB != c.MemMB {
		return fmt.Errorf("snapshot mismatch: snap %s/%dc/%dMB vs vm %s/%dc/%dMB; delete the snapshot or recreate the vm identically",
			m.Arch, m.CPUs, m.MemMB, c.Arch, c.CPUs, c.MemMB)
	}
	if m.Accel != c.Accel {
		return fmt.Errorf("snapshot accel %s != current %s; refusing heterogeneous restore", m.Accel, c.Accel)
	}
	return nil
}

// selfExe resolves the running pvm binary for exec: migration URIs.
func selfExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// CreateSnapshot captures RAM + disk state of a running VM and leaves it
// stopped. The flow is transactional: any failure after the guest is
// paused resumes it.
func (in *Instance) CreateSnapshot(vmDir, pvmVersion string) error {
	in.mu.Lock()
	defer in.mu.Unlock()

	st := in.Running()
	if st == nil {
		return fmt.Errorf("vm %s is not running", in.Cfg.Name)
	}
	sp := SnapPaths(vmDir)

	q, err := DialQMP(in.Cfg.QMPSock)
	if err != nil {
		return fmt.Errorf("qmp: %w", err)
	}
	defer q.Close()
	if status, err := q.Status(); err != nil || status != "running" {
		return fmt.Errorf("guest not running (status %q, err %v)", status, err)
	}

	// From here the guest may be paused: resume on any failure.
	completed := false
	sharesRemoved := false
	defer func() {
		if !completed {
			q.Cont()
			if sharesRemoved {
				// Best-effort: put the 9p front-ends back or every later
				// snapshot fails with "Device 'fs0' not found" and the
				// shares stay gone until reboot.
				in.reattachShares(q)
			}
		}
	}()

	// 9p/VirtFS blocks migration; hot-unplug the shares while the guest
	// is still running so it can complete the PCI hot-unplug (ids as
	// assigned by BuildArgs: fs0 = default share, then extra mounts).
	// The guest already lazily unmounted them via ssh (daemon side).
	for i := 0; i < 1+len(in.Cfg.ExtraMounts); i++ {
		id := fmt.Sprintf("fs%d", i)
		if err := q.DeviceDel(id, 15*time.Second); err != nil {
			return err
		}
	}
	sharesRemoved = true

	if err := q.Stop(); err != nil {
		return fmt.Errorf("qmp stop: %w", err)
	}
	// No QEMU-level compression: QEMU 11 dropped the "compress"
	// capability, and the exec: pipe already zstd-compresses.

	exe, err := selfExe()
	if err != nil {
		return err
	}
	ramTmp := sp.RAM + ".tmp"
	os.Remove(ramTmp)
	uri := fmt.Sprintf("exec:%s __snapshot-pipe-out %s", shellQuote(exe), shellQuote(ramTmp))
	if err := q.Migrate(uri); err != nil {
		return fmt.Errorf("qmp migrate: %w", err)
	}
	if err := waitMigrate(q, in.Cfg.MemMB); err != nil {
		return err
	}

	// RAM is fully saved; the old process must go away (its replacement
	// boots from the snapshot).
	q.Quit()
	waitDead(st.Ident, 10*time.Second)
	if st.Ident.Alive() {
		return fmt.Errorf("qemu still alive after migrate+quit")
	}

	// New snap base is converted beside the old one and atomically
	// renamed over it; only then is the stale restore overlay dropped
	// (its backing was the old snap).
	snapTmp := sp.Snap + ".tmp"
	os.Remove(snapTmp)
	out, err := exec.Command(in.Toolchain.Img, "convert", "-f", "qcow2", "-O", "qcow2", in.Cfg.DiskPath, snapTmp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %v\n%s", err, out)
	}
	if err := os.Rename(snapTmp, sp.Snap); err != nil {
		return err
	}
	if err := os.Rename(ramTmp, sp.RAM); err != nil {
		return err
	}

	m := &SnapshotManifest{
		Name:       in.Cfg.Name,
		Arch:       in.Cfg.Arch,
		CPUs:       in.Cfg.CPUs,
		MemMB:      in.Cfg.MemMB,
		Accel:      st.Accel,
		PVMVersion: pvmVersion,
		CreatedAt:  time.Now(),
	}
	if err := m.save(vmDir); err != nil {
		return err
	}
	// The stale restore overlay is dropped only after the manifest is on
	// disk: its content is fully captured in sp.Snap (qemu has quit), and
	// a crash before this point must not lose the only reference to the
	// VM's current data.
	if in.Cfg.DiskPath == sp.Restore {
		os.Remove(sp.Restore)
	}
	completed = true
	return nil
}

func waitMigrate(q *QMP, memMB int) error {
	timeout := 60 + memMB/32 // 4GB -> ~3min ceiling
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		status, err := q.MigrateStatus()
		if err != nil {
			return fmt.Errorf("query-migrate: %w", err)
		}
		switch status {
		case "completed":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("migration %s", status)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("migration timed out after %ds", timeout)
}

// PrepareRestore rewires the instance to boot from the snapshot: fresh
// overlay on the snap base and -incoming on the RAM stream. Returns nil
// (no restore needed) when there is no unconsumed snapshot.
func (in *Instance) PrepareRestore(vmDir string) (*SnapshotManifest, error) {
	m, err := LoadManifest(vmDir)
	if errors.Is(err, ErrNoSnapshot) || (err == nil && m.Consumed) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := m.checkMatch(&in.Cfg); err != nil {
		return nil, err
	}
	if in.Toolchain != nil && in.Toolchain.BrokenLiveRestore(in.Cfg.Arch, in.Cfg.Accel) {
		return nil, fmt.Errorf("qemu 11.0.0 has an hvf/arm64 live-restore regression; upgrade QEMU to >= 11.1.1 (brew upgrade qemu) or use the bundled runtime")
	}
	sp := SnapPaths(vmDir)
	if _, err := os.Stat(sp.RAM); err != nil {
		return nil, fmt.Errorf("snapshot ram file missing: %w", err)
	}
	if _, err := os.Stat(sp.Snap); err != nil {
		return nil, fmt.Errorf("snapshot base disk missing: %w", err)
	}

	exe, err := selfExe()
	if err != nil {
		return nil, err
	}
	// A leftover restore overlay may hold real writes from a previous
	// restore that failed after the migration (cont/reattach/save error);
	// keep it as .bak instead of silently discarding that work.
	if _, err := os.Stat(sp.Restore); err == nil {
		bak := sp.Restore + ".bak"
		os.Remove(bak)
		if err := os.Rename(sp.Restore, bak); err != nil {
			return nil, fmt.Errorf("preserve restore overlay: %w", err)
		}
	}
	out, err := exec.Command(in.Toolchain.Img, "create", "-f", "qcow2", "-b", sp.Snap, "-F", "qcow2", sp.Restore).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qemu-img create restore overlay: %v\n%s", err, out)
	}
	in.Cfg.DiskPath = sp.Restore
	in.Cfg.Incoming = fmt.Sprintf("exec:%s __snapshot-pipe-in %s", shellQuote(exe), shellQuote(sp.RAM))
	in.Cfg.NoVirtFSDevices = true
	return m, nil
}

// WaitRestored blocks until the incoming migration completes, resumes
// the guest, re-attaches the 9p shares and marks the snapshot consumed
// so the next start is a plain disk boot.
func (in *Instance) WaitRestored(vmDir string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		q, err := DialQMP(in.Cfg.QMPSock)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		defer q.Close()
		for time.Now().Before(deadline) {
			status, err := q.MigrateStatus()
			if err != nil {
				return fmt.Errorf("query-migrate: %w", err)
			}
			switch status {
			case "completed":
				// Mark consumed before anything else: the overlay now
				// holds the migrated disk state, so even a cont/reattach
				// failure must fall back to a plain boot from it rather
				// than re-restoring stale RAM over newer writes.
				m, err := LoadManifest(vmDir)
				if err != nil {
					return err
				}
				m.Consumed = true
				if err := m.save(vmDir); err != nil {
					return err
				}
				// Incoming migration can leave the guest paused;
				// an explicit cont is required (same as aq).
				if err := q.Cont(); err != nil {
					st, _ := q.Status()
					if st != "running" {
						return fmt.Errorf("cont after restore: %w", err)
					}
				}
				if err := in.reattachShares(q); err != nil {
					return err
				}
				return nil
			case "failed", "cancelled":
				return fmt.Errorf("restore migration %s", status)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	return fmt.Errorf("restore timed out")
}

// reattachShares hot-adds the 9p front-end devices omitted from the
// restore argv; the fresh hotplug event lets the guest enumerate them
// naturally (a pci rescan does not).
func (in *Instance) reattachShares(q *QMP) error {
	tags := []string{"pvmshare"}
	for _, m := range in.Cfg.ExtraMounts {
		tags = append(tags, m.Tag)
	}
	for i, tag := range tags {
		id := fmt.Sprintf("fs%d", i)
		_, err := q.Execute("device_add", map[string]any{
			"driver":    "virtio-9p-pci-non-transitional",
			"id":        id,
			"fsdev":     id,
			"mount_tag": tag,
			"bus":       fmt.Sprintf("rp%d", i),
		})
		if err != nil {
			return fmt.Errorf("reattach share %s: %w", tag, err)
		}
	}
	return nil
}

// RemoveSnapshot deletes all snapshot artefacts. The caller must ensure
// the VM is stopped.
func RemoveSnapshot(vmDir string) error {
	sp := SnapPaths(vmDir)
	for _, f := range []string{sp.RAM, sp.Snap, sp.Restore, sp.Manifest} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// shellQuote wraps s in single quotes for /bin/sh -c (qemu exec: URIs).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ActiveDisk returns the disk a stopped VM should boot from: the restore
// overlay when present (it holds post-snapshot writes), else def.
func ActiveDisk(vmDir, def string) string {
	if _, err := os.Stat(SnapPaths(vmDir).Restore); err == nil {
		return SnapPaths(vmDir).Restore
	}
	return def
}
