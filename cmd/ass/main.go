// Command ass is the Audio Slop Server: one GPU, many audio models, swapped in
// and out like a well-organized sock drawer. This file is the entrypoint —
// small on purpose, because main() should read like a table of contents.
package main

import (
	"flag"
	"log"
	"os"

	"github.com/stevelittlefish/AudioSlopServer/internal/banner"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
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

	log.Printf("loaded config from %s: %d service(s), listening on %s",
		*configPath, len(cfg.Services), cfg.Server.Addr)
	for name, svc := range cfg.Services {
		log.Printf("  service %-12s image=%s port=%d evict=%s", name, svc.Image, svc.Port, svc.Evict)
	}

	// TODO(slice 1): stand up the supervisor, job store, and HTTP API here.
	// For now we've proven the boot sequence and config load. Baby steps toward
	// world-changing slop.
	log.Printf("nothing else wired up yet — see TODO.md. Exiting gracefully instead of pretending.")
}
