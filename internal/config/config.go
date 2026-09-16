// Package config loads ASS's TOML configuration. Per the treatise (rule 4),
// configuration lives in TOML files, not a swamp of environment variables.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole of ASS's configuration, straight from the TOML file.
type Config struct {
	Memory   Memory             `toml:"memory"`
	GPU      GPU                `toml:"gpu"`
	Server   Server             `toml:"server"`
	Services map[string]Service `toml:"services"`
}

// Memory is the global RAM budget. The big server keeps everything parked; the
// peasant with 16GB sets this low and watches things fall back to "stop".
type Memory struct {
	RAMBudgetMB int `toml:"ram_budget_mb"`
}

// GPU describes the single card ASS is allowed to boss around, plus the VRAM
// accounting knobs. vram_budget_mb must cover the pinned model AND the context
// tax of every parked backend — see the architecture notes.
type GPU struct {
	Device       int `toml:"device"`
	VRAMBudgetMB int `toml:"vram_budget_mb"`
	ContextTaxMB int `toml:"context_tax_mb"`
}

// Server is where ASS itself listens. Nothing exotic.
type Server struct {
	Addr string `toml:"addr"`
}

// EvictPolicy is how ASS frees the GPU when a different model needs it. VRAM
// eviction is lazy either way; this only picks the demotion target.
type EvictPolicy string

const (
	// EvictPark keeps the container alive with weights parked in CPU RAM. Needs
	// the backend's /park + /unpark endpoints; fast to restore.
	EvictPark EvictPolicy = "park"
	// EvictStop kills the container entirely, freeing all its RAM. Slow to
	// restore, but the only sane choice for the RAM-hungry giants.
	EvictStop EvictPolicy = "stop"
)

// Service is a single audio backend ASS multiplexes onto the GPU.
type Service struct {
	Image        string      `toml:"image"`
	Port         int         `toml:"port"`
	Verb         string      `toml:"verb"`  // job verb: separate, generate, transcribe...
	Evict        EvictPolicy `toml:"evict"` // park | stop
	RAMReserveMB int         `toml:"ram_reserve_mb"`
	IdleTTL      Duration    `toml:"idle_ttl"` // 0 = never reclaim parked RAM
}

// Duration is a time.Duration that unmarshals from a TOML string like "10m",
// because TOML has no native duration type and "600000000000" helps nobody.
type Duration struct{ time.Duration }

// UnmarshalText lets BurntSushi turn "5m" into an actual duration.
func (d *Duration) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		return nil
	}
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	d.Duration = parsed
	return nil
}

// Load reads and validates the TOML config at path. It returns a helpful error
// rather than a stack trace, because we are not animals.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return &cfg, nil
}

// validate catches the config mistakes that would otherwise turn into a
// confusing runtime death an hour later.
func (c *Config) validate() error {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080" // a sensible default beats a mysterious :0
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("no [services.*] configured — ASS with nothing to serve is just S")
	}
	for name, svc := range c.Services {
		if svc.Image == "" {
			return fmt.Errorf("service %q: missing image", name)
		}
		if svc.Port == 0 {
			return fmt.Errorf("service %q: missing port", name)
		}
		switch svc.Evict {
		case EvictPark, EvictStop:
			// fine
		case "":
			// Default to the conservative choice: stop needs no cooperation
			// from the backend, park does.
			svc.Evict = EvictStop
			c.Services[name] = svc
		default:
			return fmt.Errorf("service %q: unknown evict policy %q (want park|stop)", name, svc.Evict)
		}
	}
	return nil
}
