package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"github.com/huangzheng2016/pvm/internal/gateway"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__counters" {
		counters, err := gateway.ReadDropCounters()
		if err != nil {
			log.Fatal(err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(counters)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "__grant" {
		fs := flag.NewFlagSet("__grant", flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/pvm/gateway.json", "gateway config")
		vmName := fs.String("vm", "", "vm name")
		seconds := fs.Int("seconds", 90, "lease seconds")
		_ = fs.Parse(os.Args[2:])
		cfg, err := gateway.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		if err := gateway.GrantLease(cfg, *vmName, *seconds); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "__probe-exit" {
		fs := flag.NewFlagSet("__probe-exit", flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/pvm/gateway.json", "gateway config")
		exitName := fs.String("exit", "direct", "exit name or direct")
		geo := fs.Bool("geo", false, "include geo lookup")
		_ = fs.Parse(os.Args[2:])
		cfg, err := gateway.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := gateway.ProbeExit(ctx, cfg, *exitName, *geo)
		if err != nil {
			log.Fatal(err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	configPath := flag.String("config", "/etc/pvm/gateway.json", "config path pushed by the host daemon")
	flag.Parse()
	log.SetPrefix("pvm-gateway: ")
	log.Printf("starting, config=%s", *configPath)
	if err := gateway.New(*configPath).Run(); err != nil {
		log.Fatal(err)
	}
}
