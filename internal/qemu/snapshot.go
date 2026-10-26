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
	"github.com/huangzheng2016/pvm/internal/native"
)

// RAM snapshot layout inside the VM directory:
//
//	state.snapshot.zst  RAM + device state, zstd compressed
//	disk.snap.qcow2     flattened standalone base captured at snapshot time
//	disk.restore.qcow2  per-restore overlay backed by disk.snap.qcow2
//	snapshot.json       manifest
//
// Snapshot creation must resume a paused guest on failure and replace the
// old snap base only after the new conversion completes.
type SnapshotManifest struct {
	Name        string    `json:"name"`
	Arch        string    `json:"arch"`
	CPUs        int       `json:"cpus"`
	MemMB       int       `json:"mem_mb"`
	Accel       Accel     `json:"accel"`
	GPU         bool      `json:"gpu"`
	Balloon     bool      `json:"balloon"`
	QEMUVersion string    `json:"qemu_version"`
	PVMVersion  string    `json:"pvm_version"`
	CreatedAt   time.Time `json:"created_at"`
	Consumed    bool      `json:"consumed"`
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

// checkMatch refuses a heterogeneous restore (migration streams are only
// guaranteed within one qemu runtime).
func (m *SnapshotManifest) checkMatch(c *VMConfig, qemuVersion string) error {
	if m.Arch != native.Arch || m.CPUs != c.CPUs || m.MemMB != c.MemMB {
		return fmt.Errorf("snapshot mismatch: snap %s/%dc/%dMB vs vm %s/%dc/%dMB; delete the snapshot or recreate the vm identically",
			m.Arch, m.CPUs, m.MemMB, native.Arch, c.CPUs, c.MemMB)
	}
	if m.Accel != c.Accel {
		fallback, ok := m.Accel.Fallback()
		if !ok || fallback != c.Accel {
			return fmt.Errorf("snapshot accel %s is incompatible with current %s; refusing heterogeneous restore", m.Accel, c.Accel)
		}
		// Restore with the accelerator recorded in the snapshot; Start may
		// still perform its normal early-failure fallback to that same
		// value, so an hvf snapshot on a tcg host is a valid restore.
		c.Accel = m.Accel
	}
	if m.GPU != c.GPU {
		return fmt.Errorf("snapshot gpu layout (%v) differs from the vm (%v); heterogeneous restore refused, delete the snapshot", m.GPU, c.GPU)
	}
	if m.Balloon != c.Balloon {
		return fmt.Errorf("snapshot balloon layout (%v) differs from the vm (%v); heterogeneous restore refused, delete the snapshot", m.Balloon, c.Balloon)
	}
	if m.QEMUVersion != qemuVersion {
		return fmt.Errorf("snapshot was written by qemu %s, current runtime is %s; migration across qemu versions is not guaranteed, delete the snapshot", m.QEMUVersion, qemuVersion)
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
// stopped. Failures before QEMU exits resume the guest; later failures
// retain its current disk for a cold restart.
func (in *Instance) CreateSnapshot(vmDir, pvmVersion string) (retErr error) {
	in.mu.Lock()
	defer in.mu.Unlock()

	st, err := in.Running()
	if err != nil {
		return fmt.Errorf("cannot snapshot vm safely: %w", err)
	}
	if st == nil {
		return fmt.Errorf("vm %s is not running", in.Cfg.Name)
	}
	sp := SnapPaths(vmDir)
	if err := CheckSnapshotCreate(vmDir); err != nil {
		return err
	}

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
	removedTags := []string{}
	migrationStarted := false
	processStopped := false
	defer func() {
		if completed {
			return
		}
		if processStopped {
			retErr = fmt.Errorf("snapshot failed after VM stopped; current disk retained for restart: %w", retErr)
			return
		}
		if migrationStarted {
			if _, err := q.Execute("migrate_cancel", nil); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("cancel migration: %w", err))
			}
		}
		if err := q.Cont(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("resume after failed snapshot: %w", err))
		}
		for _, tag := range removedTags {
			slot, _ := VirtFSSlot(tag)
			if err := q.DeviceAddVirtFS(slot, tag); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("reattach share %s after failed snapshot: %w", tag, err))
			}
		}
	}()

	// 9p/VirtFS blocks migration; hot-unplug the shares while the guest
	// is still running so it can complete the PCI hot-unplug. The guest
	// already lazily unmounted them via ssh (daemon side).
	slots := []int{0}
	for _, m := range in.Cfg.ExtraMounts {
		slot, err := VirtFSSlot(m.Tag)
		if err != nil {
			return err
		}
		slots = append(slots, slot)
	}
	for _, slot := range slots {
		if err := q.DeviceDel(VirtFSID(slot), 15*time.Second); err != nil {
			return err
		}
		tag := "pvmshare"
		if slot > 0 {
			tag = fmt.Sprintf("pvm%d", slot-1)
		}
		removedTags = append(removedTags, tag)
	}

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
	migrationStarted = true
	if err := waitMigrate(q, in.Cfg.MemMB); err != nil {
		return err
	}

	// RAM is fully saved; the old process must go away (its replacement
	// boots from the snapshot).
	if err := q.Quit(); err != nil {
		return fmt.Errorf("quit after migration: %w", err)
	}
	waitDead(st.Ident, 10*time.Second)
	if st.Ident.Alive() {
		return fmt.Errorf("qemu still alive after migrate+quit")
	}

	processStopped = true

	// Capture the stopped disk before publishing the snapshot manifest.
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
		Name:        in.Cfg.Name,
		Arch:        native.Arch,
		CPUs:        in.Cfg.CPUs,
		MemMB:       in.Cfg.MemMB,
		Accel:       st.Accel,
		GPU:         in.Cfg.GPU,
		Balloon:     in.Cfg.Balloon,
		QEMUVersion: in.Toolchain.versionString(),
		PVMVersion:  pvmVersion,
		CreatedAt:   time.Now(),
	}
	if err := m.save(vmDir); err != nil {
		return err
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

func (t *Toolchain) versionString() string {
	if t == nil || t.Version == [3]int{} {
		return "unknown"
	}
	return fmt.Sprintf("%d.%d.%d", t.Version[0], t.Version[1], t.Version[2])
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
	if err := m.checkMatch(&in.Cfg, in.Toolchain.versionString()); err != nil {
		return nil, err
	}
	if in.Toolchain != nil && in.Toolchain.BrokenLiveRestore(in.Cfg.Accel) {
		return nil, fmt.Errorf("qemu 11.0.0 has an hvf/arm64 live-restore regression; upgrade QEMU to >= 11.1.0 (brew upgrade qemu) or use the bundled runtime")
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
		if _, err := os.Lstat(bak); err == nil {
			return nil, fmt.Errorf("restore backup already exists; inspect and move %s aside before retrying, or cold-start the VM from its current restore disk", bak)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect restore backup: %w", err)
		}
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
	for _, tag := range tags {
		slot, err := VirtFSSlot(tag)
		if err != nil {
			return err
		}
		if err := q.DeviceAddVirtFS(slot, tag); err != nil {
			return fmt.Errorf("reattach share %s: %w", tag, err)
		}
	}
	return nil
}

// RemoveSnapshot deletes all snapshot artefacts. The caller must ensure
// the VM is stopped.
func RemoveSnapshot(vmDir string) error {
	sp := SnapPaths(vmDir)
	for _, path := range []string{sp.Restore, sp.Restore + ".bak"} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("cannot remove snapshot: restore overlay or backup holds VM data; clone the VM or preserve and flatten the restore disk before removing its backing snapshot")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
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

func CheckSnapshotCreate(vmDir string) error {
	sp := SnapPaths(vmDir)
	for _, path := range []string{sp.Restore, sp.Manifest, sp.RAM, sp.Snap} {
		if _, err := os.Lstat(path); err == nil {
			if path == sp.Restore {
				return fmt.Errorf("cannot snapshot an active restore overlay; clone the VM into a new VM before creating another snapshot")
			}
			return fmt.Errorf("snapshot artifacts already exist; remove the existing snapshot explicitly before creating another")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect snapshot artifacts: %w", err)
		}
	}
	return nil
}
