package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/mihomo"
	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/proxyimport"
)

func mihomoSourcePath(home, name string) string {
	return filepath.Join(home, "exits", "sources", name+".yaml")
}

func mihomoPortForName(home, name string) int {
	used := map[int]bool{}
	if entries, err := exit.ListEntries(filepath.Join(home, "exits")); err == nil {
		for _, e := range entries {
			if e.Mihomo != nil && e.Name != name {
				used[e.Mihomo.Port] = true
			}
		}
	}
	base := 17890 + int(crc32.ChecksumIEEE([]byte(name))%1000)
	for i := 0; i < 1000; i++ {
		p := 17890 + (base-17890+i)%1000
		if !used[p] {
			return p
		}
	}
	return 17890
}

func (s *Server) handleExitImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		Content string   `json:"content_b64"`
		Select  string   `json:"select"`
		Chain   []string `json:"chain"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if !exit.ValidName(req.Name) {
		writeErr(w, 400, fmt.Errorf("invalid exit name %q", req.Name))
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		writeErr(w, 400, fmt.Errorf("invalid content_b64: %w", err))
		return
	}
	src, err := proxyimport.Parse(req.Name, raw)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	selected := req.Select
	if len(req.Chain) > 0 {
		selected = req.Chain[len(req.Chain)-1]
	}
	if selected == "" {
		selected = src.Proxies[0]["name"].(string)
	}
	port := mihomoPortForName(s.P.Home, req.Name)
	if old, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), req.Name); err == nil && old.Mihomo != nil {
		port = old.Mihomo.Port
	}
	rendered, err := proxyimport.BuildMihomoConfig(src, selected, req.Chain, port)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	srcBytes, _ := json.MarshalIndent(src, "", "  ")
	if err := os.MkdirAll(filepath.Dir(mihomoSourcePath(s.P.Home, req.Name)), 0o700); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := fsutil.AtomicWrite(mihomoSourcePath(s.P.Home, req.Name), srcBytes, 0o600); err != nil {
		writeErr(w, 500, err)
		return
	}
	entry := &exit.Entry{
		Name: req.Name, Provider: "mihomo", Transport: "tcp",
		Mihomo:    &exit.MihomoSpec{Selected: selected, Chain: req.Chain, Port: port},
		CreatedAt: time.Now(),
	}
	if err := exit.SaveEntry(filepath.Join(s.P.Home, "exits"), entry); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.deployMihomoToGateway(req.Name, rendered); err != nil {
		_ = exit.RemoveEntry(filepath.Join(s.P.Home, "exits"), req.Name)
		_ = os.Remove(mihomoSourcePath(s.P.Home, req.Name))
		writeErr(w, 502, fmt.Errorf("deploy mihomo exit (rolled back local registration): %w", err))
		return
	}
	writeJSON(w, 201, map[string]any{
		"name": req.Name, "provider": "mihomo", "selected": selected, "chain": req.Chain,
		"nodes": len(src.Proxies), "skipped": src.Skipped,
	})
}

func (s *Server) deployMihomoToGateway(name string, rendered []byte) error {
	cl := s.Clusters.Get(1)
	if cl == nil {
		return fmt.Errorf("no cluster gateway yet; create a VM first")
	}
	gw, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	binPath, err := mihomo.EnsureBinary(s.P.Runtime, native.Arch, nil)
	if err != nil {
		return err
	}
	tmpBin := "/tmp/pvm-mihomo.new"
	if err := s.scpTo(gw, binPath, tmpBin); err != nil {
		return fmt.Errorf("upload mihomo binary: %w", err)
	}
	cfgRemote := fmt.Sprintf("/tmp/pvm-mihomo-%s.yaml", name)
	cfgTmp, err := os.CreateTemp("", "pvm-mihomo-cfg-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(cfgTmp.Name())
	if _, err := cfgTmp.Write(rendered); err != nil {
		cfgTmp.Close()
		return err
	}
	cfgTmp.Close()
	if err := s.scpTo(gw, cfgTmp.Name(), cfgRemote); err != nil {
		return fmt.Errorf("upload mihomo config: %w", err)
	}

	unit := fmt.Sprintf(`[Unit]
Description=pvm Mihomo proxy core (%s)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
DynamicUser=yes
StateDirectory=pvm-mihomo-%s
ExecStart=/usr/local/bin/pvm-mihomo -d /var/lib/pvm-mihomo-%s -f /etc/pvm/mihomo/%s.yaml
Restart=always
RestartSec=3
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadOnlyPaths=/etc/pvm/mihomo

[Install]
WantedBy=multi-user.target
`, name, name, name, name)
	unitB64 := base64.StdEncoding.EncodeToString([]byte(unit))
	installCmd := fmt.Sprintf(
		"sudo install -m 0755 %s /usr/local/bin/pvm-mihomo && rm -f %s && "+
			"sudo install -d -m 0700 -o root -g root /etc/pvm/mihomo && "+
			"sudo install -m 0600 -o root -g root %s /etc/pvm/mihomo/%s.yaml && rm -f %s && "+
			"echo %s | base64 -d | sudo tee /etc/systemd/system/pvm-mihomo-%s.service >/dev/null && "+
			"sudo systemctl daemon-reload && sudo systemctl enable --now pvm-mihomo-%s.service && "+
			"sleep 1 && sudo systemctl is-active --quiet pvm-mihomo-%s.service",
		shellQuote(tmpBin), shellQuote(tmpBin),
		shellQuote(cfgRemote), name, shellQuote(cfgRemote),
		unitB64, name, name, name)
	out, err := s.sshRun(gw, installCmd)
	if err != nil {
		return fmt.Errorf("install mihomo service: %v\n%s", err, strings.TrimSpace(out))
	}
	return s.pushGatewayConfig(cl.ID)
}

func (s *Server) handleMihomoNodes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !exit.ValidName(name) {
		writeErr(w, 400, fmt.Errorf("invalid exit name"))
		return
	}
	raw, err := os.ReadFile(mihomoSourcePath(s.P.Home, name))
	if err != nil {
		writeErr(w, 404, fmt.Errorf("mihomo source %q not found", name))
		return
	}
	var src proxyimport.Source
	if err := json.Unmarshal(raw, &src); err != nil {
		writeErr(w, 500, err)
		return
	}
	names := make([]string, 0, len(src.Proxies))
	for _, p := range src.Proxies {
		names = append(names, fmt.Sprint(p["name"]))
	}
	writeJSON(w, 200, map[string]any{"name": name, "nodes": names, "skipped": src.Skipped})
}

func (s *Server) handleMihomoRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !exit.ValidName(name) {
		writeErr(w, 400, fmt.Errorf("invalid exit name"))
		return
	}
	if cl := s.Clusters.Get(1); cl != nil {
		if gw, err := s.getSpec(cl.Gateway); err == nil {
			_, _ = s.sshRun(gw, fmt.Sprintf("sudo systemctl disable --now pvm-mihomo-%s.service; sudo rm -f /etc/systemd/system/pvm-mihomo-%s.service /etc/pvm/mihomo/%s.yaml; sudo systemctl daemon-reload", name, name, name))
		}
	}
	_ = os.Remove(mihomoSourcePath(s.P.Home, name))
	writeJSON(w, 200, map[string]any{"removed": name})
}
