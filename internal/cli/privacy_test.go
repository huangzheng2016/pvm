package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestPrivacyAuditRedactsAndDoesNotInitializeState(t *testing.T) {
	p := paths.New(filepath.Join(t.TempDir(), "absent"))
	var out bytes.Buffer
	if printAudit(&out, privacyChecks(p, "dev1")) != 1 {
		t.Fatal("missing state must fail")
	}
	if _, err := os.Stat(p.Home); !os.IsNotExist(err) {
		t.Fatalf("audit created state: %v", err)
	}
	if strings.Contains(out.String(), p.Home) || strings.Contains(out.String(), "dev1") {
		t.Fatalf("private identifier leaked: %s", out.String())
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.VMDir("dev1"), 0700); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", ShareMode: vm.ShareGlobal, Mounts: []vm.GuestMount{{Host: "/private/example", Guest: "/work"}}}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printAudit(&out, privacyChecks(p, "dev1"))
	for _, private := range []string{p.Home, "/private/example", "dev1"} {
		if strings.Contains(out.String(), private) {
			t.Fatalf("private identifier leaked: %s", private)
		}
	}
	if !strings.Contains(out.String(), "share=warn") {
		t.Fatalf("missing share warning: %s", out.String())
	}
}

func TestRouteExplanationUsesRulePriorityWithoutResolving(t *testing.T) {
	cl := &cluster.Cluster{DefaultExit: "my-exit", VMExits: map[string]string{"dev1": "direct"}, Rules: []rules.Rule{
		{Type: rules.CIDR, Pattern: "203.0.113.0/24", Action: rules.Direct},
		{Type: rules.DomainSuffix, Pattern: "example.com", Action: rules.Proxy, Exit: "my-exit"},
	}}
	for _, tt := range []struct{ destination, want string }{
		{"example.com", "proxy (explicit rule"},
		{"203.0.113.1", "direct (explicit rule"},
		{"dev1.pvm.internal", "cluster-local"},
		{"unmatched.test", "direct (VM override"},
	} {
		got, err := routeExplanation(cl, "dev1", tt.destination)
		if err != nil || !strings.Contains(got, tt.want) {
			t.Fatalf("%s: %s, %v", tt.destination, got, err)
		}
		if strings.Contains(got, "my-exit") {
			t.Fatal("exit identifier leaked")
		}
	}
}

func TestMountReadOnlyFlag(t *testing.T) {
	options, err := parseMountAddArgs([]string{"dev1", "--read-only", "/host:/guest"})
	if err != nil || !options.readOnly {
		t.Fatalf("options=%+v err=%v", options, err)
	}
}
