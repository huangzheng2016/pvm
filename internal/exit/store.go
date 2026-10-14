package exit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`)

// ValidName reports whether name is safe to use as an exit identifier
// (it becomes a filename under the exits dir).
func ValidName(name string) bool { return nameRe.MatchString(name) }

func entryPath(dir, name string) string {
	return filepath.Join(dir, name+".json")
}

const ProviderWARP = "warp"

type WARPAccount struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
	License  string `json:"license,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
}

type Entry struct {
	Name         string       `json:"name"`
	Provider     string       `json:"provider,omitempty"`
	Server       string       `json:"server"` // control plane host:port
	Token        string       `json:"token"`
	Transport    string       `json:"transport"` // udp | tcp
	Fingerprint  string       `json:"fingerprint"`
	WGPrivateKey string       `json:"wg_private_key"`
	WGPublicKey  string       `json:"wg_public_key"`
	ServerPubKey string       `json:"server_pubkey"`
	TunnelIP     string       `json:"tunnel_ip"`
	ServerIP     string       `json:"server_ip,omitempty"` // tunnel-side .1 (speed/ping target)
	MTU          int          `json:"mtu"`
	Endpoint     string       `json:"endpoint"` // wireguard data-plane host:port
	WARP         *WARPAccount `json:"warp,omitempty"`
	// Region profile: applied to VMs routed via this exit so their
	// environment matches the exit's geography. TZ is the manual
	// override (exit set --tz); AutoTZ is learned from the exit-agent's
	// enroll response and used when TZ is empty.
	TZ        string    `json:"tz,omitempty"`
	AutoTZ    string    `json:"auto_tz,omitempty"`
	Locale    string    `json:"locale,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (e *Entry) IsWARP() bool { return e.Provider == ProviderWARP }

// SaveEntry persists an entry (0600).
func SaveEntry(dir string, e *Entry) error {
	if !ValidName(e.Name) {
		return fmt.Errorf("invalid exit name %q", e.Name)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(entryPath(dir, e.Name), b, 0o600)
}

func LoadEntry(dir, name string) (*Entry, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid exit name %q", name)
	}
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

func RemoveEntry(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid exit name %q", name)
	}
	err := os.Remove(entryPath(dir, name))
	if os.IsNotExist(err) {
		return fmt.Errorf("exit %q not found", name)
	}
	return err
}
