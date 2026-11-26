package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/diaglog"
	"github.com/huangzheng2016/pvm/internal/paths"
)

const diagUsage = "usage: pvm diag [output.zip] [--offline]"

func parseDiagArgs(args []string) (out string, offline bool, err error) {
	flags := flag.NewFlagSet("diag", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&offline, "offline", false, "skip live daemon and gateway collection")
	var flagArgs, positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	if err := flags.Parse(flagArgs); err != nil || len(positional) > 1 {
		return "", false, fmt.Errorf("%s", diagUsage)
	}
	if len(positional) == 1 {
		out = positional[0]
	}
	return out, offline, nil
}

func CmdDiag(args []string) int {
	args = scanJSONFlag(args)
	outArg, offline, err := parseDiagArgs(args)
	if err != nil {
		return fatalf("%v", err)
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	out := ""
	if outArg != "" {
		out, err = filepath.Abs(outArg)
		if err != nil {
			return fatalf("output path: %v", err)
		}
	} else {
		out = filepath.Join(mustCwd(), fmt.Sprintf("pvm-diag-%s.zip", time.Now().Format("20060102-150405")))
	}

	var online func() (map[string][]byte, error)
	if !offline {
		online = func() (map[string][]byte, error) { return collectOnlineDiag(p) }
	}
	m, err := diaglog.ExportBundle(p, out, online)
	if err != nil {
		return fatalf("diag: %v", err)
	}
	if src, ok := m.Sources["daemon"]; ok {
		fmt.Fprintf(os.Stderr, "note: live daemon collection unavailable (%s)\n", src)
	}
	if jsonOutput {
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": map[string]any{"path": out, "files": m.Files, "manifest": m}}))
	}
	fmt.Printf("wrote %s (%d files, redacted; review before sharing)\n", out, m.Files)
	return 0
}

func mustCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func collectOnlineDiag(p *paths.Paths) (map[string][]byte, error) {
	c := daemon.NewClient(p)
	if !c.Healthy() {
		return nil, fmt.Errorf("daemon is not running")
	}
	out := map[string][]byte{}
	for _, ep := range []struct{ api, name string }{
		{"/v1/status", "daemon/status.json"},
		{"/v1/vms", "daemon/vms.json"},
		{"/v1/routes", "daemon/routes.json"},
	} {
		var raw json.RawMessage
		if err := c.Get(ep.api, &raw); err != nil {
			return out, fmt.Errorf("get %s: %w", ep.api, err)
		}
		pretty, err := json.MarshalIndent(json.RawMessage(raw), "", "  ")
		if err != nil {
			pretty = raw
		}
		out[ep.name] = pretty
	}
	var clusters []struct {
		ID int `json:"id"`
	}
	if err := c.Get("/v1/clusters", &clusters); err == nil {
		for _, cl := range clusters {
			var gw struct {
				Gateway string `json:"gateway"`
				Log     string `json:"log"`
			}
			if err := c.Get(fmt.Sprintf("/v1/diag/gateway/%d", cl.ID), &gw); err == nil && gw.Log != "" {
				out[fmt.Sprintf("clusters/%d/%s-gateway.log", cl.ID, gw.Gateway)] = []byte(gw.Log)
			}
		}
	}
	return out, nil
}
