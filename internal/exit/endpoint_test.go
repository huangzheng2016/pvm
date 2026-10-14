package exit

import (
	"testing"

	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestDialEntryResolvesHostnameEndpoint(t *testing.T) {
	private, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := tunnel.PublicKey(private)
	if err != nil {
		t.Fatal(err)
	}
	client, err := DialEntry(&Entry{
		Transport:    "udp",
		WGPrivateKey: private,
		ServerPubKey: public,
		TunnelIP:     "10.99.0.2",
		Endpoint:     "localhost:9",
		MTU:          1420,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
}
