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
	Storage  Storage            `toml:"storage"`
	Docker   Docker             `toml:"docker"`
	Web      Web                `toml:"web"`
	Services map[string]Service `toml:"services"`
}

// Web governs the human-facing web console and its operator controls (park /
// stop / unload-all a backend by hand). It rides the same listener as the API
// (Server.Addr), so it inherits the same 0.0.0.0 bind — there's no auth yet, so
// it's LAN-only in spirit. On by default; flip it off on a box you'd rather keep
// strictly API-with-no-buttons.
type Web struct {
	// Enabled is a *bool so an omitted [web] table means "on" (a plain bool
	// would default to false). nil => enabled; set `enabled = false` to disable.
	Enabled *bool `toml:"enabled"`
}

// WebEnabled reports whether the console + operator endpoints should be served.
// Absent config = enabled.
func (c *Config) WebEnabled() bool {
	return c.Web.Enabled == nil || *c.Web.Enabled
}

// Storage is where ASS keeps its own state: the job database and the harvested
// artifact bytes. Sensible defaults so a minimal config just works.
type Storage struct {
	DBPath     string `toml:"db_path"`
	ResultsDir string `toml:"results_dir"`
}

// Docker is how ASS reaches the daemon. Empty socket = the platform default.
type Docker struct {
	Socket string `toml:"socket"`
}

// Memory is the global RAM budget. The big server keeps everything parked; the
// peasant with 16GB sets this low and watches things fall back to "stop".
type Memory struct {
	RAMBudgetMB int `toml:"ram_budget_mb"`
}

// GPU describes the single card ASS is allowed to boss around, plus the VRAM
// accounting knobs. vram_budget_mb must cover the pinned model AND the context
// tax of every parked backend — see the architecture notes.
//
// Enabled is the on/off switch: on this GPU-less dev box (and a peasant laptop)
// it's false, so ASS requests no GPU and everything still runs. On the real
// server it's true with device = 0.
type GPU struct {
	Enabled      bool `toml:"enabled"`
	Device       int  `toml:"device"`
	VRAMBudgetMB int  `toml:"vram_budget_mb"`
	ContextTaxMB int  `toml:"context_tax_mb"`
	// MaxResident is how many backends may hold the GPU (be pinned) at once. The
	// whole point of ASS is that this is small — default 1, "one model on the
	// card." A big multi-GPU-ish future could raise it; the arbiter honors it as
	// the pin capacity and evicts the LRU resident to stay under it.
	MaxResident int `toml:"max_resident"`
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
	Image        string            `toml:"image"`
	Container    string            `toml:"container"` // container name; defaults to "ass-<service>"
	Port         int               `toml:"port"`      // published, and passed to the backend
	Verb         string            `toml:"verb"`      // job verb: separate, generate, transcribe...
	Env          map[string]string `toml:"env"`       // extra env for the backend container
	Volumes      []string          `toml:"volumes"`   // "host:container[:ro]" mounts (weight caches etc.)
	ShmSizeMB    int               `toml:"shm_size_mb"`
	Evict        EvictPolicy       `toml:"evict"` // park | stop
	RAMReserveMB int               `toml:"ram_reserve_mb"`
	IdleTTL      Duration          `toml:"idle_ttl"` // 0 = never reclaim parked RAM
}

// ContainerName is the container name for this service, defaulting to
// "ass-<service>" when not set. The service's own name is passed in because the
// Service doesn't carry it (it's the map key).
func (s Service) ContainerName(service string) string {
	if s.Container != "" {
		return s.Container
	}
	return "ass-" + service
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
		c.Server.Addr = ":2645" // 0xA55 = "ASS" in hex. Unique, memorable, ours.
	}
	if c.Storage.DBPath == "" {
		c.Storage.DBPath = "data/ass.db"
	}
	if c.Storage.ResultsDir == "" {
		c.Storage.ResultsDir = "data/results"
	}
	if c.GPU.MaxResident == 0 {
		c.GPU.MaxResident = 1 // one model on the card, as nature intended
	}
	if c.GPU.MaxResident < 0 {
		return fmt.Errorf("gpu.max_resident %d is negative — that's fewer than no models", c.GPU.MaxResident)
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
