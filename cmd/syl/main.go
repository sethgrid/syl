package main

import (
	"log"

	"github.com/caarlos0/env/v11"

	"github.com/sethgrid/syl/server"
)

// Version is set at build time via ldflags.
var Version = "dev"

func main() {
	cfg := server.Config{}
	if err := env.Parse(&cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	cfg.Version = Version

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("create server: %v", err)
	}

	if err := srv.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
