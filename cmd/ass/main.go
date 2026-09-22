// Command ass is the Audio Slop Server: one GPU, many audio models, swapped in
// and out like a well-organized sock drawer. This file is the entrypoint —
// small on purpose, because main() should read like a table of contents.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/api"
	"github.com/stevelittlefish/AudioSlopServer/internal/arbiter"
	"github.com/stevelittlefish/AudioSlopServer/internal/banner"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/docker"
	"github.com/stevelittlefish/AudioSlopServer/internal/engine"
	"github.com/stevelittlefish/AudioSlopServer/internal/reaper"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
	"github.com/stevelittlefish/AudioSlopServer/internal/supervisor"
)

func main() {
	// The one flag we allow. Everything else lives in TOML, as the treatise
	// commands (rule 4).
	configPath := flag.String("config", "ass.toml", "path to the TOML config file")
	printImages := flag.Bool("print-images", false,
		"print each enabled service's Docker image (one per line) and exit; used by pull-services.sh")
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	// Utility mode: emit the image list and get out, no banner, no server. This is
	// the single source of truth for "which images does this config need" — same
	// config.Load as the server, so disabled services drop out for free. Keeps
	// pull-services.sh from hand-parsing TOML.
	if *printImages {
		cfg, err := config.Load(*configPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		for _, name := range sortedServiceNames(cfg) {
			fmt.Println(cfg.Services[name].Image)
		}
		return
	}

	// First things first. Non-negotiable objective #1.
	banner.Print(os.Stdout)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("loaded config from %s: %d service(s)", *configPath, len(cfg.Services))
	if len(cfg.DisabledServices) > 0 {
		log.Printf("skipping %d disabled service(s): %v", len(cfg.DisabledServices), cfg.DisabledServices)
	}

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

	// Boot from a known-empty card. Any backend still running from a previous ASS
	// process is a ghost the fresh (in-memory, empty) arbiter can't see — it holds
	// VRAM the arbiter thinks is free, so the next swap OOMs. Reap them all now so
	// the empty residency map is actually true. Non-fatal: if we can't tidy up,
	// log it and press on rather than refusing to boot.
	reapCtx, reapCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if n, err := sup.CleanSlate(reapCtx); err != nil {
		log.Printf("warning: clean slate failed (%v) — leftover backends may still hold VRAM", err)
	} else if n > 0 {
		log.Printf("reaped %d leftover backend container(s) on startup", n)
	}
	reapCancel()

	arb := arbiter.New(sup, cfg)
	eng := engine.New(cfg, arb, st, res)

	// The reaper: reclaims disk by deleting the oldest finished jobs once the
	// results store outgrows the [retention] limits. Off unless a limit is set —
	// the big server hoards forever, the peasant's box tidies up after itself.
	if cfg.Retention.Active() {
		rp := reaper.New(cfg.Retention, st, res)
		eng.OnJobDone = rp.Trigger // sweep opportunistically right after a job lands
		go rp.Run(context.Background())
		log.Printf("retention: reaper on (max_total_mb=%d max_jobs=%d max_age=%s sweep=%s)",
			cfg.Retention.TotalMB(), cfg.Retention.MaxJobs,
			cfg.Retention.MaxAge.Duration, cfg.Retention.SweepInterval.Duration)
	} else {
		log.Printf("retention: off — harvested results are kept forever (set [retention] limits to reclaim disk)")
	}

	handler := api.New(cfg, eng, st, arb).Handler()

	for name, svc := range cfg.Services {
		log.Printf("  service %-12s image=%s port=%d verb=%s evict=%s", name, svc.Image, svc.Port, svc.Verb, svc.Evict)
		if cfg.GPU.VRAMBudgetMB > 0 && svc.VRAMPinnedMB > cfg.GPU.VRAMBudgetMB {
			log.Printf("  WARNING: %s reserves %d MiB > gpu.vram_budget_mb %d — it can't be budgeted to fit; "+
				"ASS will evict everything and load it anyway, but a heavy job may OOM the card",
				name, svc.VRAMPinnedMB, cfg.GPU.VRAMBudgetMB)
		}
	}

	log.Printf("ASS listening on %s — bring me your slop", cfg.Server.Addr)
	if err := http.ListenAndServe(cfg.Server.Addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// sortedServiceNames returns the enabled service names in a stable order, so
// -print-images emits the same list every run (maps iterate randomly in Go).
func sortedServiceNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
