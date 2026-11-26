package diaglog

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/paths"
)

// RedactorForPaths registers the exact credentials and endpoints stored under
// p so log output and diagnostic bundles redact them even when they do not
// match a generic key=value pattern. Unreadable or absent state is skipped.
func RedactorForPaths(p *paths.Paths) *Redactor {
	r := NewRedactor()
	if p == nil {
		return r
	}
	r.AddPath(p.Home, "[pvm-home]")
	if home, err := os.UserHomeDir(); err == nil {
		r.AddPath(home, "[home]")
	}
	if tok, err := os.ReadFile(p.DaemonAdminToken()); err == nil {
		r.AddSecret(string(trimSpaceBytes(tok)))
	}
	exitsDir := filepath.Join(p.Home, "exits")
	if entries, err := exit.ListEntries(exitsDir); err == nil {
		for _, e := range entries {
			r.AddSecret(e.Token)
			r.AddSecret(e.WGPrivateKey)
			r.AddEndpoint(hostOnly(e.Server))
			r.AddEndpoint(hostOnly(e.Endpoint))
			r.AddEndpoint(e.ServerIP)
			if e.WARP != nil {
				r.AddSecret(e.WARP.Token)
				r.AddSecret(e.WARP.License)
				r.AddEndpoint(e.WARP.IPv6)
			}
		}
	}
	if fis, err := os.ReadDir(exitsDir); err == nil {
		for _, fi := range fis {
			if filepath.Ext(fi.Name()) != ".warp-pending" {
				continue
			}
			if raw, err := os.ReadFile(filepath.Join(exitsDir, fi.Name())); err == nil {
				var pending struct {
					Token string `json:"token"`
				}
				if json.Unmarshal(raw, &pending) == nil {
					r.AddSecret(pending.Token)
				}
			}
		}
	}
	if fis, err := os.ReadDir(p.VMs); err == nil {
		for _, fi := range fis {
			if !fi.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(p.VMs, fi.Name(), "desktop.json"))
			if err != nil {
				continue
			}
			var st struct {
				Password string `json:"password"`
			}
			if json.Unmarshal(raw, &st) == nil {
				r.AddSecret(st.Password)
			}
		}
	}
	return r
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func trimSpaceBytes(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}
