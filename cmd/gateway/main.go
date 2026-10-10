package main

import (
	"flag"
	"log"

	"github.com/huangzheng2016/pvm/internal/gateway"
)

func main() {
	configPath := flag.String("config", "/etc/pvm/gateway.json", "config path pushed by the host daemon")
	flag.Parse()
	log.SetPrefix("pvm-gateway: ")
	log.Printf("starting, config=%s", *configPath)
	if err := gateway.New(*configPath).Run(); err != nil {
		log.Fatal(err)
	}
}
