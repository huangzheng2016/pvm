package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type mountRequest struct {
	Host    string `json:"host"`
	Guest   string `json:"guest"`
	Profile string `json:"profile"`
}

func (s *Server) handleMountList(w http.ResponseWriter, r *http.Request) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	mounts := append([]vm.GuestMount(nil), spec.Mounts...)
	if mounts == nil {
		mounts = []vm.GuestMount{}
	}
	for i := range mounts {
		mounts[i].Profile, err = vm.NormalizeMountProfile(mounts[i].Profile)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	writeJSON(w, 200, mounts)
}

func (s *Server) handleMountAdd(w http.ResponseWriter, r *http.Request) {
	var req mountRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	m, err := newGuestMount(spec, req.Host, req.Guest, req.Profile)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.addMount(spec, m); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) handleMountRemove(w http.ResponseWriter, r *http.Request) {
	guest := r.URL.Query().Get("guest")
	if guest == "" {
		writeErr(w, 400, fmt.Errorf("guest mount path is required"))
		return
	}
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	index := -1
	var m vm.GuestMount
	for i, candidate := range spec.Mounts {
		if candidate.Guest == guest {
			index, m = i, candidate
			break
		}
	}
	if index < 0 {
		writeErr(w, 404, fmt.Errorf("mount %q not found on %s", guest, spec.Name))
		return
	}
	if err := s.removeMount(spec, m); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]string{"removed": guest})
}

func newGuestMount(spec *vm.Spec, host, guest, profile string) (vm.GuestMount, error) {
	if spec.Role == "gateway" {
		return vm.GuestMount{}, fmt.Errorf("gateway mounts are managed by pvm")
	}
	if host == "" || guest == "" {
		return vm.GuestMount{}, fmt.Errorf("host and guest paths are required")
	}
	profile, err := vm.NormalizeMountProfile(profile)
	if err != nil {
		return vm.GuestMount{}, err
	}
	host, err = filepath.Abs(host)
	if err != nil {
		return vm.GuestMount{}, err
	}
	fi, err := os.Stat(host)
	if err != nil || !fi.IsDir() {
		return vm.GuestMount{}, fmt.Errorf("mount host path %s is not a directory", host)
	}
	if guest == "/share" {
		return vm.GuestMount{}, fmt.Errorf("/share is reserved for the default pvm share")
	}
	if err := vm.ValidateSpec(&vm.Spec{User: spec.User}, []vm.GuestMount{{Guest: guest, Profile: profile}}); err != nil {
		return vm.GuestMount{}, err
	}
	used := make([]bool, qemu.MaxExtraMounts)
	for _, existing := range spec.Mounts {
		if existing.Guest == guest {
			return vm.GuestMount{}, fmt.Errorf("guest path %s is already mounted", guest)
		}
		slot, err := qemu.VirtFSSlot(existing.Tag)
		if err != nil || slot == 0 {
			return vm.GuestMount{}, fmt.Errorf("invalid existing mount tag %q", existing.Tag)
		}
		used[slot-1] = true
	}
	for i := range used {
		if !used[i] {
			return vm.GuestMount{Tag: fmt.Sprintf("pvm%d", i), Host: host, Guest: guest, Profile: profile}, nil
		}
	}
	return vm.GuestMount{}, fmt.Errorf("too many mounts (max %d)", qemu.MaxExtraMounts)
}

func (s *Server) addMount(spec *vm.Spec, m vm.GuestMount) error {
	mounts := append(append([]vm.GuestMount(nil), spec.Mounts...), m)
	next := *spec
	next.Mounts = mounts
	inst := s.instance(spec)
	st, err := inst.Running()
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("vm %s is not running; start it before changing mounts", spec.Name)
	}
	slot, _ := qemu.VirtFSSlot(m.Tag)
	if !dynamicMountSlotReady(st.Args, slot) {
		return fmt.Errorf("vm %s needs one restart to enable dynamic mounts; run `pvm stop %s && pvm start %s`", spec.Name, spec.Name, spec.Name)
	}
	if out, err := s.sshRun(spec, fmt.Sprintf("if mountpoint -q %s; then echo %s >&2; exit 1; fi", shellQuote(m.Guest), shellQuote("mountpoint is already in use: "+m.Guest))); err != nil {
		return fmt.Errorf("check guest mountpoint: %v%s", err, indentOutput(out))
	}
	mountScript, err := guestMountAddScript(m)
	if err != nil {
		return err
	}
	if err := s.prepareMountSlots(&next); err != nil {
		return err
	}
	q, err := qemu.DialQMP(inst.Cfg.QMPSock)
	if err != nil {
		return s.mountStateError(fmt.Errorf("qmp: %w", err), s.prepareMountSlots(spec))
	}
	defer q.Close()
	if err := q.DeviceAddVirtFS(slot, m.Tag); err != nil {
		return s.mountStateError(err, s.prepareMountSlots(spec))
	}
	deviceAdded := true
	if out, err := s.sshRun(spec, mountScript); err != nil {
		rollback := s.rollbackAddedMount(spec, m, q, deviceAdded)
		return s.mountStateError(fmt.Errorf("mount in guest: %v%s", err, indentOutput(out)), rollback)
	}
	if err := next.Save(s.P); err != nil {
		return s.mountStateError(err, s.rollbackAddedMount(spec, m, q, deviceAdded))
	}
	spec.Mounts = mounts
	return nil
}

func (s *Server) removeMount(spec *vm.Spec, m vm.GuestMount) error {
	mounts := make([]vm.GuestMount, 0, len(spec.Mounts)-1)
	for _, existing := range spec.Mounts {
		if existing.Tag != m.Tag {
			mounts = append(mounts, existing)
		}
	}
	next := *spec
	next.Mounts = mounts
	inst := s.instance(spec)
	st, err := inst.Running()
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("vm %s is not running; start it before changing mounts", spec.Name)
	}
	if out, err := s.sshRun(spec, guestMountRemoveScript(m)); err != nil {
		return fmt.Errorf("unmount in guest; configuration retained, retry after fixing SSH/guest state: %v%s", err, indentOutput(out))
	}
	q, err := qemu.DialQMP(inst.Cfg.QMPSock)
	if err != nil {
		return fmt.Errorf("guest unmounted but qmp is unavailable; configuration retained, retry the remove: %w", err)
	}
	defer q.Close()
	slot, _ := qemu.VirtFSSlot(m.Tag)
	if err := q.DeviceDel(qemu.VirtFSID(slot), 15*time.Second); err != nil && !deviceNotFound(err) {
		return fmt.Errorf("guest unmounted but device hot-unplug failed; configuration retained, retry the remove: %w", err)
	}
	if err := s.prepareMountSlots(&next); err != nil {
		return fmt.Errorf("mount detached but backend cleanup failed; configuration retained, retry the remove: %w", err)
	}
	if err := next.Save(s.P); err != nil {
		return fmt.Errorf("mount detached but configuration save failed; configuration retained, retry the remove: %w", err)
	}
	spec.Mounts = mounts
	return nil
}

func (s *Server) rollbackAddedMount(spec *vm.Spec, m vm.GuestMount, q *qemu.QMP, deviceAdded bool) error {
	return s.rollbackAddedMountWith(spec, m, q, deviceAdded, s.sshRun)
}

func (s *Server) rollbackAddedMountWith(spec *vm.Spec, m vm.GuestMount, q *qemu.QMP, deviceAdded bool, run func(*vm.Spec, string) (string, error)) error {
	if deviceAdded {
		if out, err := run(spec, guestMountRemoveScript(m)); err != nil {
			return s.retainFailedMount(spec, m, fmt.Errorf("guest rollback: %v%s", err, indentOutput(out)))
		}
		slot, _ := qemu.VirtFSSlot(m.Tag)
		if err := q.DeviceDel(qemu.VirtFSID(slot), 15*time.Second); err != nil && !deviceNotFound(err) {
			return s.retainFailedMount(spec, m, fmt.Errorf("device rollback: %w", err))
		}
	}
	if err := s.prepareMountSlots(spec); err != nil {
		return s.retainFailedMount(spec, m, fmt.Errorf("backend rollback: %w", err))
	}
	return nil
}

func (s *Server) retainFailedMount(spec *vm.Spec, m vm.GuestMount, cause error) error {
	next := *spec
	next.Mounts = append(append([]vm.GuestMount(nil), spec.Mounts...), m)
	spec.Mounts = next.Mounts
	if err := next.Save(s.P); err != nil {
		return fmt.Errorf("%w; mount backend retained but recovery configuration could not be saved: %v; stop the VM before retrying", cause, err)
	}
	return fmt.Errorf("%w; mount configuration and backend retained; retry `pvm mount rm %s %s`", cause, spec.Name, m.Guest)
}

func (s *Server) mountStateError(err, rollback error) error {
	if rollback != nil {
		return fmt.Errorf("%w; rollback failed: %v", err, rollback)
	}
	return err
}

func (s *Server) prepareMountSlots(spec *vm.Spec) error {
	if spec.Role == "gateway" {
		return nil
	}
	dir := s.P.MountDir(spec.Name)
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		return err
	}
	targets := make(map[int]string, len(spec.Mounts))
	for _, m := range spec.Mounts {
		slot, err := qemu.VirtFSSlot(m.Tag)
		if err != nil || slot == 0 {
			return fmt.Errorf("invalid extra mount tag %q", m.Tag)
		}
		if targets[slot] != "" {
			return fmt.Errorf("duplicate mount tag %q", m.Tag)
		}
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("mount host path %s is not absolute", m.Host)
		}
		fi, err := os.Stat(m.Host)
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("mount host path %s is not a directory", m.Host)
		}
		targets[slot] = m.Host
	}
	type update struct {
		path, target, previous string
		existed                bool
	}
	var updates []update
	for slot := 1; slot <= qemu.MaxExtraMounts; slot++ {
		target := targets[slot]
		if target == "" {
			target = empty
		}
		change := update{path: s.P.MountSlot(spec.Name, slot), target: target}
		fi, err := os.Lstat(change.path)
		if err == nil {
			if fi.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("mount slot %s is not a daemon-managed symlink", change.path)
			}
			change.previous, err = os.Readlink(change.path)
			if err != nil {
				return err
			}
			change.existed = true
			if change.previous == target {
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		updates = append(updates, change)
	}
	for i, change := range updates {
		if err := ensureMountSymlink(change.path, change.target); err != nil {
			errs := []error{err}
			for j := i - 1; j >= 0; j-- {
				previous := updates[j]
				current, readErr := os.Readlink(previous.path)
				if readErr != nil || current != previous.target {
					errs = append(errs, fmt.Errorf("cannot restore changed mount slot %s", previous.path))
					continue
				}
				var restoreErr error
				if previous.existed {
					restoreErr = ensureMountSymlink(previous.path, previous.previous)
				} else {
					restoreErr = os.Remove(previous.path)
				}
				if restoreErr != nil {
					errs = append(errs, fmt.Errorf("restore mount slot: %w", restoreErr))
				}
			}
			return errors.Join(errs...)
		}
	}

	return nil
}

func ensureMountSymlink(path, target string) error {
	fi, err := os.Lstat(path)
	if err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("mount slot %s is not a daemon-managed symlink", path)
		}
		current, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if current == target {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-mount-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if err := os.Symlink(target, tmpPath); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func dynamicMountSlotReady(args []string, slot int) bool {
	joined := strings.Join(args, " ")
	return strings.Contains(joined, "id="+qemu.VirtFSID(slot)+",") &&
		strings.Contains(joined, "id="+qemu.VirtFSRootPort(slot)+",")
}

func guestMountAddScript(m vm.GuestMount) (string, error) {
	options, err := vm.GuestMountOptions(m.Profile)
	if err != nil {
		return "", err
	}
	line := fmt.Sprintf("%s %s 9p %s,rw,_netdev,nofail 0 0", m.Tag, m.Guest, options)
	// QMP returns before the guest necessarily enumerates the hotplugged tag.
	return "set -e\n" +
		"for attempt in 1 2 3 4 5; do\n" +
		"  if grep -qsx " + shellQuote(m.Tag) + " /sys/bus/virtio/devices/virtio*/mount_tag 2>/dev/null; then break; fi\n" +
		"  sleep 1\n" +
		"done\n" +
		"sudo sed -i " + shellQuote("/^"+m.Tag+" /d") + " /etc/fstab\n" +
		"sudo mkdir -p " + shellQuote(m.Guest) + "\n" +
		"sudo mount -t 9p -o " + shellQuote(options) + " " + shellQuote(m.Tag) + " " + shellQuote(m.Guest) + "\n" +
		"printf '%s\\n' " + shellQuote(line) + " | sudo tee -a /etc/fstab >/dev/null", nil
}

func guestMountRemoveScript(m vm.GuestMount) string {
	return "set -e\n" +
		"if mountpoint -q " + shellQuote(m.Guest) + "; then sudo umount " + shellQuote(m.Guest) + "; fi\n" +
		"sudo sed -i " + shellQuote("/^"+m.Tag+" /d") + " /etc/fstab"
}

func deviceNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

func indentOutput(out string) string {
	if strings.TrimSpace(out) == "" {
		return ""
	}
	return "\n" + strings.TrimSpace(out)
}
