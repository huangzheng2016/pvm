package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/exit"
)

func exitImport(args []string) int {
	fs := newFlagSet("exit import", "<name> --file <config.yaml|-> [--select NODE] [--chain relay,exit]")
	file := fs.String("file", "", "Clash/Mihomo/Surge YAML or share-link file (- for stdin)")
	selectNode := fs.String("select", "", "exit node name (default: first node)")
	chain := fs.String("chain", "", "comma-separated proxy chain from first hop to exit")
	args = scanJSONFlag(args)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *file == "" {
		return fatalf("usage: pvm exit import <name> --file <config.yaml|-> [--select NODE] [--chain relay,exit]")
	}
	var raw []byte
	var err error
	if *file == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, (8<<20)+1))
	} else {
		raw, err = os.ReadFile(*file)
	}
	if err != nil {
		return fatalf("read proxy config: %v", err)
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	body := map[string]any{
		"name":        fs.Arg(0),
		"content_b64": base64.StdEncoding.EncodeToString(raw),
		"select":      *selectNode,
	}
	if *chain != "" {
		body["chain"] = splitCSV(*chain)
	}
	var out struct {
		Name     string `json:"name"`
		Selected string `json:"selected"`
		Nodes    int    `json:"nodes"`
		Skipped  []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := c.Post("/v1/exits/import", body, &out); err != nil {
		return fatalf("%v", err)
	}
	if jsonOutput {
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": out}))
	}
	fmt.Printf("imported exit %q (%d usable nodes, selected %q)\n", out.Name, out.Nodes, out.Selected)
	for _, sk := range out.Skipped {
		fmt.Fprintf(os.Stderr, "  skipped %s: %s\n", sanitizeText(sk.Name), sk.Reason)
	}
	fmt.Printf("route a VM with: pvm route dev1 --via %s\n", out.Name)
	return 0
}

func exitNodes(args []string) int {
	args = scanJSONFlag(args)
	if len(args) != 1 {
		return fatalf("usage: pvm exit nodes <name>")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var out struct {
		Name  string   `json:"name"`
		Nodes []string `json:"nodes"`
	}
	if err := c.Get("/v1/exits/"+url.PathEscape(args[0])+"/nodes", &out); err != nil {
		return fatalf("%v", err)
	}
	if jsonOutput {
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": out}))
	}
	for _, n := range out.Nodes {
		fmt.Println(sanitizeText(n))
	}
	return 0
}

func removeMihomoExitIfNeeded(c *daemon.Client, e *exit.Entry) error {
	if e == nil || !e.IsMihomo() {
		return nil
	}
	return c.Delete("/v1/exits/"+url.PathEscape(e.Name)+"/mihomo", nil)
}
