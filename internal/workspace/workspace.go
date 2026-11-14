// Package workspace manages directories shared between one host folder and
// one VM. See docs/design/workspace.md.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

type Mode string

const (
	ModeMount Mode = "mount"
	ModeSync  Mode = "sync"
	ModeCopy  Mode = "copy"
)

// Mount transports. 9p reuses the daemon's `pvm mount` path; sftp is the
// reverse sshfs path for hosts whose QEMU has no virtio-9p (native Windows).
const (
	Transport9p   = "9p"
	TransportSFTP = "sftp"
)

// GuestRoot is the parent of every workspace inside the guest.
const GuestRoot = "/workspace"

// StagingDir is the guest directory uploads are extracted into before
// they are renamed into place. Scans skip it.
const StagingDir = GuestRoot + "/.pvm-staging"

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var ErrNotFound = errors.New("workspace not found")

func ValidID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid workspace id %q: use 1-63 lowercase letters, digits and '-', starting with a letter or digit", id)
	}
	return nil
}

func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeMount, ModeSync, ModeCopy:
		return m, nil
	}
	return "", fmt.Errorf("invalid mode %q: use mount, sync or copy", s)
}

func GuestPathFor(id string) string { return GuestRoot + "/" + id }

type Workspace struct {
	ID        string    `json:"id"`
	VM        string    `json:"vm"`
	Mode      Mode      `json:"mode"`
	HostPath  string    `json:"host_path"`
	GuestPath string    `json:"guest_path"`
	ReadOnly  bool      `json:"read_only,omitempty"`
	Transport string    `json:"transport,omitempty"` // mount mode only
	Created   time.Time `json:"created"`
	// LastTransfer is the time of the last upload, push, pull or clone;
	// the baseline manifest describes the tree as of that moment.
	LastTransfer time.Time `json:"last_transfer,omitzero"`
}

// EntryType is the kind of a tree entry. Anything else (devices, FIFOs,
// sockets) is never transferred.
type EntryType string

const (
	TypeFile    EntryType = "file"
	TypeDir     EntryType = "dir"
	TypeSymlink EntryType = "symlink"
)

// Entry describes one path of a workspace tree. Path is slash-separated
// and relative to the workspace root, without a leading "./".
type Entry struct {
	Path   string    `json:"path"`
	Type   EntryType `json:"type"`
	Size   int64     `json:"size,omitempty"`
	MTime  int64     `json:"mtime,omitempty"` // unix nanoseconds
	SHA256 string    `json:"sha256,omitempty"`
	Exec   bool      `json:"exec,omitempty"`
	Link   string    `json:"link,omitempty"` // symlink target
}

// SameContent reports whether two entries hold the same content; size and
// modification time are only a hashing shortcut and do not count.
func (e Entry) SameContent(o Entry) bool {
	if e.Type != o.Type {
		return false
	}
	switch e.Type {
	case TypeFile:
		return e.SHA256 == o.SHA256 && e.Exec == o.Exec
	case TypeSymlink:
		return e.Link == o.Link
	}
	return true
}

// Manifest is a scanned tree, keyed by Entry.Path.
type Manifest map[string]Entry

func (m Manifest) Sorted() []Entry {
	out := make([]Entry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Baseline records the tree both sides agreed on after the last transfer,
// with each side's size and mtime so unchanged files need no rehash.
type Baseline struct {
	Content Manifest `json:"content"`
	Host    Manifest `json:"host"`
	Guest   Manifest `json:"guest"`
}

// Matcher decides whether a relative slash path is excluded from a
// workspace. Excluding a directory excludes everything below it.
type Matcher interface {
	Excluded(rel string, isDir bool) bool
}

type Store struct{ Dir string }

func NewStore(p *paths.Paths) *Store { return &Store{Dir: p.Workspaces} }

func (s *Store) metaPath(id string) string     { return filepath.Join(s.Dir, id+".json") }
func (s *Store) baselinePath(id string) string { return filepath.Join(s.Dir, id+".baseline.json") }

func (s *Store) ensure() error { return os.MkdirAll(s.Dir, 0o700) }

func (s *Store) Load(id string) (*Workspace, error) {
	if err := ValidID(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.metaPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var ws Workspace
	if err := json.Unmarshal(b, &ws); err != nil {
		return nil, fmt.Errorf("workspace %s: %w", id, err)
	}
	if ws.ID != id {
		return nil, fmt.Errorf("workspace %s: metadata names %q", id, ws.ID)
	}
	return &ws, nil
}

func (s *Store) Save(ws *Workspace) error {
	if err := ValidID(ws.ID); err != nil {
		return err
	}
	if err := s.ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.metaPath(ws.ID), append(b, '\n'), 0o600)
}

func (s *Store) List() ([]*Workspace, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Workspace
	for _, e := range ents {
		name := e.Name()
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || strings.HasSuffix(id, ".baseline") || ValidID(id) != nil {
			continue
		}
		ws, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Delete removes the metadata and baseline. It never touches workspace
// files on either side.
func (s *Store) Delete(id string) error {
	if err := ValidID(id); err != nil {
		return err
	}
	if err := os.Remove(s.baselinePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(s.metaPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	os.Remove(filepath.Join(s.Dir, id+".lock"))
	return nil
}

// LoadBaseline returns nil, nil when no baseline was recorded yet.
func (s *Store) LoadBaseline(id string) (*Baseline, error) {
	b, err := os.ReadFile(s.baselinePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bl Baseline
	if err := json.Unmarshal(b, &bl); err != nil {
		return nil, fmt.Errorf("workspace %s baseline: %w", id, err)
	}
	return &bl, nil
}

func (s *Store) SaveBaseline(id string, bl *Baseline) error {
	if err := s.ensure(); err != nil {
		return err
	}
	b, err := json.Marshal(bl)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.baselinePath(id), b, 0o600)
}

// Lock serializes commands on one workspace across processes.
func (s *Store) Lock(id string) (*fsutil.Lock, error) {
	if err := ValidID(id); err != nil {
		return nil, err
	}
	if err := s.ensure(); err != nil {
		return nil, err
	}
	l, err := fsutil.TryLock(filepath.Join(s.Dir, id+".lock"))
	if err != nil {
		return nil, fmt.Errorf("workspace %s is busy with another pvm command: %w", id, err)
	}
	return l, nil
}
