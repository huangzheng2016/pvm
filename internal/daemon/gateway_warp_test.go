package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
)

func TestGatewayConfigWARPDataPlaneOnly(t *testing.T) {
	s := newPushTestServer(t)
	if _, err := s.Clusters.Ensure(1); err != nil {
		t.Fatal(err)
	}
	if err := exit.SaveEntry(s.P.Home+"/exits", &exit.Entry{
		Name:         "warp-test",
		Provider:     exit.ProviderWARP,
		Server:       "api.cloudflareclient.com",
		Transport:    "udp",
		WGPrivateKey: "client-private",
		ServerPubKey: "server-public",
		TunnelIP:     "172.16.0.2",
		MTU:          1280,
		Endpoint:     "203.0.113.10:2408",
		WARP:         &exit.WARPAccount{DeviceID: "device-1", Token: "cloudflare-api-token"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Clusters.SetDefaultExit(1, "warp-test"); err != nil {
		t.Fatal(err)
	}
	raw := gatewayConfigFromCluster(s, s.Clusters.Get(1))
	if strings.Contains(string(raw), "cloudflare-api-token") {
		t.Fatal("Cloudflare API token must not be pushed to the gateway")
	}
	var doc struct {
		Exits map[string]struct {
			Provider     string `json:"provider"`
			WGPrivateKey string `json:"wg_private_key"`
			ServerPubKey string `json:"server_pubkey"`
			TunnelIP     string `json:"tunnel_ip"`
			MTU          int    `json:"mtu"`
			Endpoint     string `json:"endpoint"`
			DNS          string `json:"dns"`
		} `json:"exits"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	def := doc.Exits["warp-test"]
	if def.Provider != exit.ProviderWARP || def.WGPrivateKey != "client-private" || def.ServerPubKey != "server-public" || def.TunnelIP != "172.16.0.2" || def.MTU != 1280 || def.Endpoint != "203.0.113.10:2408" || def.DNS != "1.1.1.1" {
		t.Fatalf("unexpected WARP definition: %+v", def)
	}
}
