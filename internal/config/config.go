// Package config loads ASS's TOML configuration. Per the treatise (rule 4),
// configuration lives in TOML files, not a swamp of environment variables.
package config

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole of ASS's configuration, straight from the TOML file.
type Config struct {
	Memory    Memory             `toml:"memory"`
	GPU       GPU                `toml:"gpu"`
	Server    Server             `toml:"server"`
	Storage   Storage            `toml:"storage"`
	Retention Retention          `toml:"retention"`
	Docker    Docker             `toml:"docker"`
	Web       Web                `toml:"web"`
	Services  map[string]Service `toml:"services"`

	// DisabledServices names the [services.*] entries dropped because they set
	// disabled = true. Not from the TOML — filled in by validate() after it
	// removes them from Services, purely so startup can log what it skipped.
	DisabledServices []string `toml:"-"`
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

// Retention governs the reaper: how long ASS hoards harvested job results before
// deleting the oldest to reclaim disk. The size cap defaults ON — 15 GB — because
// a box that silently fills its disk is worse than one that quietly forgets last
// month's slop (empirically ~80 MB/job, so ~8 GB per 100 jobs). Raise it, add the
// other limits, or set max_total_mb = 0 to hoard forever. Only DONE jobs
// (succeeded/failed) are ever reaped; live work is untouchable, and within a
// sweep the OLDEST-finished go first.
type Retention struct {
	// MaxTotalMB caps the total size of all harvested artifacts on disk. When the
	// results store grows past this, the reaper deletes oldest-first until it's back
	// under. It's a *int64 so we can tell "unset" (nil -> the 15000 MB default)
	// apart from an explicit 0 (unlimited, hoard forever). The knob that matters
	// most for audio.
	MaxTotalMB *int64 `toml:"max_total_mb"`
	// MaxJobs caps how many terminal jobs are kept, oldest deleted beyond it. 0 =
	// unlimited.
	MaxJobs int `toml:"max_jobs"`
	// MaxAge deletes any terminal job that finished longer ago than this. 0 = keep
	// forever regardless of age.
	MaxAge Duration `toml:"max_age"`
	// SweepInterval is how often the reaper wakes to enforce the above (it also runs
	// once at startup and opportunistically after each job completes, so age limits
	// still get caught between ticks). Defaults to 10m when any limit is set; ignored
	// entirely when retention is off.
	SweepInterval Duration `toml:"sweep_interval"`
}

// TotalMB is the effective size budget in MB: the configured value, or 0 when it's
// been explicitly disabled. Only call after validate() has filled the default —
// before that a nil pointer reads as 0 (no budget yet), which is why validate
// sets the default before anything consults this.
func (r Retention) TotalMB() int64 {
	if r.MaxTotalMB == nil {
		return 0
	}
	return *r.MaxTotalMB
}

// Active reports whether the reaper has anything to enforce — any of the three
// limits set. With the default 15 GB size cap this is normally true; it's false
// only when every limit is off (max_total_mb = 0 and no count/age limit).
func (r Retention) Active() bool {
	return r.TotalMB() > 0 || r.MaxJobs > 0 || r.MaxAge.Duration > 0
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
	// Disabled removes this service from ASS entirely: it isn't registered, can't
	// be started or addressed, never shows in /v1/backends or the web console, and
	// pull-services.sh skips its image. A disabled block is inert config — the rest
	// of its fields aren't even validated — so you can park a half-configured
	// service here without breaking startup. Default false: the service is live.
	Disabled bool `toml:"disabled"`

	Image        string            `toml:"image"`
	Container    string            `toml:"container"` // container name; defaults to "ass-<service>"
	Port         int               `toml:"port"`      // published, and passed to the backend
	Verb         string            `toml:"verb"`      // job verb: separate, generate, transcribe...
	Env          map[string]string `toml:"env"`       // extra env for the backend container
	Volumes      []string          `toml:"volumes"`   // "host:container[:ro]" mounts (weight caches etc.)
	Command      []string          `toml:"command"`   // override the image's default CMD (e.g. Stable Audio's --lora-ckpt-path)
	ShmSizeMB    int               `toml:"shm_size_mb"`
	Evict        EvictPolicy       `toml:"evict"` // park | stop
	RAMReserveMB int               `toml:"ram_reserve_mb"`
	IdleTTL      Duration          `toml:"idle_ttl"` // 0 = never reclaim parked RAM

	// Priority biases which resident gets evicted first when the card is full.
	// HIGHER = more valuable = evicted LAST. The arbiter evicts the lowest
	// priority zero-lease resident, breaking ties by least-recently-used. Leave
	// everything equal (the default) and you get pure LRU, the old behaviour.
	// Bump one service up and it survives swaps until nothing cheaper is resident.
	Priority int `toml:"priority"`

	// NoPreload keeps this service out of "Preload all" (POST /v1/backends/
	// preload-all). Preload warms every parkable backend into RAM ahead of time;
	// set this on a backend you'd rather not pay to warm eagerly — a rarely-used
	// one, or a RAM hog on a tight box — and it stays stopped until a real job
	// wants it. Only meaningful for evict = "park" (stop services never preload
	// anyway). Default false: preload warms it like everything else.
	NoPreload bool `toml:"no_preload"`

	// VRAMPinnedMB is what this service holds on the card while it's pinned. Use
	// the MEASURED PEAK (the "max" from scripts/measure-vram.sh), not the idle
	// floor — the budget has to survive the worst moment of concurrent inference,
	// and since backends load lazily we reserve the declared cost the instant a
	// backend is pinned rather than waiting to watch real usage. THIS is the knob
	// to raise if you get OOM errors.
	VRAMPinnedMB int `toml:"vram_pinned_mb"`
	// VRAMParkedMB is the context tax this service holds while parked (weights on
	// CPU, CUDA context + workspaces still on the card). Only meaningful for
	// evict = "park"; a stopped backend holds nothing. Defaults to gpu.context_tax_mb
	// when left 0 on a park service, and is forced to 0 for a stop service.
	VRAMParkedMB int `toml:"vram_parked_mb"`
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
	// Default the size cap ON at 15 GB when the key is absent, so a box with no
	// [retention] table still tidies up instead of filling its disk. An explicit
	// max_total_mb = 0 opts back out (hoard forever).
	if c.Retention.MaxTotalMB == nil {
		def := int64(15000)
		c.Retention.MaxTotalMB = &def
	}
	if *c.Retention.MaxTotalMB < 0 {
		return fmt.Errorf("retention.max_total_mb %d is negative", *c.Retention.MaxTotalMB)
	}
	if c.Retention.MaxJobs < 0 {
		return fmt.Errorf("retention.max_jobs %d is negative", c.Retention.MaxJobs)
	}
	if c.Retention.MaxAge.Duration < 0 {
		return fmt.Errorf("retention.max_age %s is negative", c.Retention.MaxAge.Duration)
	}
	// Only default the sweep cadence when retention is actually in use; a wholly
	// unset [retention] stays inert (no reaper, no timer) rather than spinning a
	// goroutine to enforce nothing. "0" explicitly disables just the periodic sweep.
	if c.Retention.Active() && c.Retention.SweepInterval.Duration == 0 {
		c.Retention.SweepInterval.Duration = 10 * time.Minute
	}
	// Two ways to gate the card. When vram_budget_mb is set, VRAM MB is the real
	// limit and max_resident is an optional secondary cap (0 = unlimited, let the
	// budget decide). When no budget is given, we fall back to the old count gate,
	// so max_resident defaults to 1 — "one model on the card, as nature intended."
	if c.GPU.VRAMBudgetMB == 0 && c.GPU.MaxResident == 0 {
		c.GPU.MaxResident = 1
	}
	if c.GPU.MaxResident < 0 {
		return fmt.Errorf("gpu.max_resident %d is negative — that's fewer than no models", c.GPU.MaxResident)
	}
	if c.GPU.VRAMBudgetMB < 0 {
		return fmt.Errorf("gpu.vram_budget_mb %d is negative", c.GPU.VRAMBudgetMB)
	}
	// Drop disabled services before anything else looks at the map, so the rest of
	// ASS never learns they existed. Their remaining fields go unvalidated on
	// purpose — a disabled block is allowed to be incomplete.
	c.DisabledServices = nil
	for name, svc := range c.Services {
		if svc.Disabled {
			c.DisabledServices = append(c.DisabledServices, name)
			delete(c.Services, name)
		}
	}
	sort.Strings(c.DisabledServices)

	if len(c.Services) == 0 {
		if len(c.DisabledServices) > 0 {
			return fmt.Errorf("every [services.*] is disabled — nothing left to serve (enable one)")
		}
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

		// VRAM accounting only matters when a budget is in force. Then every
		// service must declare what it costs pinned, or the budget math treats it
		// as free and happily overcommits the card into an OOM.
		if c.GPU.VRAMBudgetMB > 0 && svc.VRAMPinnedMB <= 0 {
			return fmt.Errorf("service %q: vram_pinned_mb must be set when gpu.vram_budget_mb is (measure it with scripts/measure-vram.sh)", name)
		}
		// Parked tax: a stop service never parks, so it holds nothing; a park
		// service defaults to the global context tax when it hasn't measured its own.
		if svc.Evict == EvictStop {
			svc.VRAMParkedMB = 0
		} else if svc.VRAMParkedMB == 0 {
			svc.VRAMParkedMB = c.GPU.ContextTaxMB
		}
		c.Services[name] = svc
	}
	return nil
}
