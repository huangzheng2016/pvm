package diaglog

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"time"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/paths"
)

const (
	daemonLogTail  = 256 << 10
	consoleLogTail = 128 << 10
	qemuLogTail    = 64 << 10
	maxBundleBytes = 64 << 20
)

var vmDirNameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

type Manifest struct {
	Format    string            `json:"format"`
	CreatedAt time.Time         `json:"created_at"`
	Version   string            `json:"version"`
	GOOS      string            `json:"goos"`
	GOARCH    string            `json:"goarch"`
	Sources   map[string]string `json:"sources"`
	Files     int               `json:"files"`
}

// ExportBundle writes a redacted, whitelist-only ZIP of host and guest
// diagnostics to outPath atomically. online may return additional entries
// collected from a live daemon; nil runs fully offline.
func ExportBundle(p *paths.Paths, outPath string, online func() (map[string][]byte, error)) (*Manifest, error) {
	if p == nil {
		return nil, fmt.Errorf("paths are required")
	}
	if outPath == "" {
		return nil, fmt.Errorf("output path is required")
	}
	if fi, err := os.Stat(outPath); err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to overwrite non-regular file %s", outPath)
	}
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := RedactorForPaths(p)
	m := &Manifest{
		Format:    "pvm-diag/1",
		CreatedAt: time.Now().UTC(),
		Version:   buildinfo.Version,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		Sources:   map[string]string{},
	}

	tmp, err := os.CreateTemp(dir, ".pvm-diag-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	zw := zip.NewWriter(tmp)
	var total int64
	add := func(name string, data []byte) error {
		if total+int64(len(data)) > maxBundleBytes {
			return fmt.Errorf("diagnostic bundle exceeds %d bytes", maxBundleBytes)
		}
		total += int64(len(data))
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: m.CreatedAt}
		hdr.SetMode(0o600)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		m.Files++
		return nil
	}
	addRedacted := func(name string, data []byte) error {
		return add(name, []byte(r.Redact(string(data))))
	}
	addJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return addRedacted(name, data)
	}

	collectFileTail := func(entryName, path string, limit int64) {
		data, err := readTail(path, limit)
		if err != nil {
			m.Sources[entryName] = err.Error()
			return
		}
		if err := addRedacted(entryName, data); err != nil {
			m.Sources[entryName] = err.Error()
			return
		}
		m.Sources[entryName] = fmt.Sprintf("collected last %d bytes (max %d)", len(data), limit)
	}

	collectFileTail("logs/daemon.log", p.DaemonLog(), daemonLogTail)

	type mountProjection struct {
		Guest    string `json:"guest"`
		Profile  string `json:"profile,omitempty"`
		ReadOnly bool   `json:"read_only,omitempty"`
	}
	type vmProjection struct {
		Name              string            `json:"name"`
		Role              string            `json:"role"`
		Image             string            `json:"image"`
		Golden            int               `json:"golden,omitempty"`
		CPUs              int               `json:"cpus"`
		MemMB             int               `json:"mem_mb"`
		DiskGB            int               `json:"disk_gb"`
		ClusterID         int               `json:"cluster_id,omitempty"`
		IP                string            `json:"ip,omitempty"`
		ShareMode         string            `json:"share_mode,omitempty"`
		Mounts            []mountProjection `json:"mounts,omitempty"`
		TZ                string            `json:"tz,omitempty"`
		Locale            string            `json:"locale,omitempty"`
		KernelArgsPending bool              `json:"kernel_args_pending,omitempty"`
		IdentityReset     bool              `json:"identity_reset,omitempty"`
		CreatedAt         time.Time         `json:"created_at"`
	}

	var vms []vmProjection
	vmDirs, err := os.ReadDir(p.VMs)
	if err != nil && !os.IsNotExist(err) {
		m.Sources["vms"] = err.Error()
	}
	sort.Slice(vmDirs, func(i, j int) bool { return vmDirs[i].Name() < vmDirs[j].Name() })
	for _, fi := range vmDirs {
		name := fi.Name()
		if !fi.IsDir() || !vmDirNameRE.MatchString(name) {
			continue
		}
		vmDir := filepath.Join(p.VMs, name)
		raw, err := os.ReadFile(filepath.Join(vmDir, "vm.json"))
		if err != nil {
			m.Sources["vms/"+name+"/spec.json"] = err.Error()
			continue
		}
		var vp vmProjection
		if err := json.Unmarshal(raw, &vp); err != nil {
			m.Sources["vms/"+name+"/spec.json"] = "corrupt vm.json: " + err.Error()
		} else {
			vms = append(vms, vp)
			if err := addJSON("vms/"+name+"/spec.json", vp); err != nil {
				return nil, err
			}
			m.Sources["vms/"+name+"/spec.json"] = "collected (whitelist projection)"
		}
		collectFileTail("vms/"+name+"/console.log", filepath.Join(vmDir, "console.log"), consoleLogTail)
		collectFileTail("vms/"+name+"/console.log.qemu", filepath.Join(vmDir, "console.log.qemu"), qemuLogTail)
	}

	type exitSummary struct {
		Name      string `json:"name"`
		Provider  string `json:"provider,omitempty"`
		Transport string `json:"transport"`
		TZ        string `json:"tz,omitempty"`
		AutoTZ    string `json:"auto_tz,omitempty"`
		Locale    string `json:"locale,omitempty"`
	}
	var exits []exitSummary
	if entries, err := exit.ListEntries(filepath.Join(p.Home, "exits")); err == nil {
		for _, e := range entries {
			exits = append(exits, exitSummary{Name: e.Name, Provider: e.Provider, Transport: e.Transport, TZ: e.TZ, AutoTZ: e.AutoTZ, Locale: e.Locale})
		}
	} else {
		m.Sources["exits"] = err.Error()
	}
	if err := addJSON("host/summary.json", map[string]any{"vm_count": len(vms), "vms": vms, "exits": exits}); err != nil {
		return nil, err
	}

	if online != nil {
		extras, err := online()
		if err != nil {
			m.Sources["daemon"] = err.Error()
		} else {
			names := make([]string, 0, len(extras))
			for k := range extras {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if err := addRedacted(k, extras[k]); err != nil {
					return nil, err
				}
				m.Sources[k] = "collected from daemon"
			}
		}
	}

	if err := addJSON("manifest.json", m); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return nil, err
	}
	return m, nil
}

func readTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if fi.Size() > limit {
		start = fi.Size() - limit
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if i := indexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	return data, nil
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
