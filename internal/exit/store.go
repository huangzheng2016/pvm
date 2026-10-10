package exit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

// Entry is a locally stored exit-node enrollment (client side).
type Entry struct {
	Name         string    `json:"name"`
	Server       string    `json:"server"` // control plane host:port
	Token        string    `json:"token"`
	Transport    string    `json:"transport"` // udp | tcp
	Fingerprint  string    `json:"fingerprint"`
	WGPrivateKey string    `json:"wg_private_key"`
	WGPublicKey  string    `json:"wg_public_key"`
	ServerPubKey string    `json:"server_pubkey"`
	TunnelIP     string    `json:"tunnel_ip"`
	MTU          int       `json:"mtu"`
	Endpoint     string    `json:"endpoint"` // wireguard data-plane host:port
	// Region profile: applied to VMs routed via this exit so their
	// environment matches the exit's geography.
	TZ        string    `json:"tz,omitempty"`
	Locale    string    `json:"locale,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func entryPath(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

// SaveEntry persists an entry (0600).
func SaveEntry(dir string, e *Entry) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(entryPath(dir, e.Name), b, 0o600)
}

// LoadEntry reads one entry by name.
func LoadEntry(dir, name string) (*Entry, error) {
	b, err := os.ReadFile(entryPath(dir, name))
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("parse entry %s: %w", name, err)
	}
	return &e, nil
}

// ListEntries returns all entries sorted by name.
func ListEntries(dir string) ([]*Entry, error) {
	fis, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, fi := range fis {
		if filepath.Ext(fi.Name()) != ".json" {
			continue
		}
		e, err := LoadEntry(dir, fi.Name()[:len(fi.Name())-5])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveEntry deletes the local entry file.
func RemoveEntry(dir, name string) error {
	err := os.Remove(entryPath(dir, name))
	if os.IsNotExist(err) {
		return fmt.Errorf("exit %q not found", name)
	}
	return err
}
