// Command ass is the Audio Slop Server: one GPU, many audio models, swapped in
// and out like a well-organized sock drawer. This file is the entrypoint —
// small on purpose, because main() should read like a table of contents.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/stevelittlefish/AudioSlopServer/internal/api"
	"github.com/stevelittlefish/AudioSlopServer/internal/arbiter"
	"github.com/stevelittlefish/AudioSlopServer/internal/banner"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/docker"
	"github.com/stevelittlefish/AudioSlopServer/internal/engine"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
	"github.com/stevelittlefish/AudioSlopServer/internal/supervisor"
)

func main() {
	// The one flag we allow. Everything else lives in TOML, as the treatise
	// commands (rule 4).
	configPath := flag.String("config", "ass.toml", "path to the TOML config file")
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	// First things first. Non-negotiable objective #1.
	banner.Print(os.Stdout)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("loaded config from %s: %d service(s)", *configPath, len(cfg.Services))

	// Our own state: the job database and the harvested-artifact store.
	st, err := store.Open(cfg.Storage.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	res, err := results.New(cfg.Storage.ResultsDir)
	if err != nil {
		log.Fatalf("results: %v", err)
	}

	// The daemon we boss around. ASS needs no GPU itself — only backends do.
	dcli, err := docker.New(cfg.Docker.Socket)
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	log.Printf("talking to docker (API %s)", dcli.APIVersion())

	sup := supervisor.New(dcli, cfg)
	arb := arbiter.New(sup, cfg)
	eng := engine.New(cfg, arb, st, res)
	handler := api.New(cfg, eng, st, arb).Handler()

	for name, svc := range cfg.Services {
		log.Printf("  service %-12s image=%s port=%d verb=%s evict=%s", name, svc.Image, svc.Port, svc.Verb, svc.Evict)
	}

	log.Printf("ASS listening on %s — bring me your slop", cfg.Server.Addr)
	if err := http.ListenAndServe(cfg.Server.Addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}
