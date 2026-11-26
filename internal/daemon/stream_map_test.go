package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/cluster"
)

func TestGatewayConfigAndForwardsExcludeStreamMaps(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	store, err := cluster.LoadStore(s.P.Clusters)
	if err != nil {
		t.Fatal(err)
	}
	s.Clusters = store
	cl, err := store.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddVM(1, spec.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddStreamMap(1, spec.Name, "100.99.1.10", 18080, 8080); err != nil {
		t.Fatal(err)
	}
	cl = store.Get(1)
	raw := gatewayConfigFromCluster(s, cl)
	var doc struct {
		Maps []map[string]any `json:"maps"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Maps) != 0 || strings.Contains(string(raw), `"host_port": 0`) {
		t.Fatalf("stream map leaked into gateway config: %s", raw)
	}
	if m := cl.Maps[0]; m.Transport != "stream" || m.PoolPort != 0 {
		t.Fatalf("unexpected stream map record: %+v", m)
	}
}
