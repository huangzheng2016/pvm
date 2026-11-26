package gateway

import "testing"

func TestParseDropCountersWithStringRuleRefs(t *testing.T) {
	raw := []byte(`{"nftables":[
		{"metainfo":{"version":"1.0.9"}},
		{"counter":{"family":"inet","name":"pvm_drop_meta","table":"pvm","handle":9,"packets":7,"bytes":420}},
		{"rule":{"family":"inet","table":"pvm","chain":"forward","expr":[{"match":{"left":{"payload":{"field":"daddr"}},"right":"169.254.0.0/16"}},{"counter":"pvm_drop_meta"},{"drop":null}]}}
	]}`)
	m, err := parseDropCounters(raw)
	if err != nil || m["pvm_drop_meta"] != 7 {
		t.Fatalf("parseDropCounters = %v, %v", m, err)
	}
}
