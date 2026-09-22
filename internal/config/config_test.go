package config

import (
	"strings"
	"testing"
)

// The configs we ship must actually load and validate — a typo'd budget or a
// service missing vram_pinned_mb under a budget shouldn't be discovered on the
// server an hour into a deploy.
func TestShippedConfigsValidate(t *testing.T) {
	for _, path := range []string{"../../ass.toml", "../../ass.dev.toml"} {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// TestBudgetRequiresPinnedCost proves the OOM-guard: with a VRAM budget in force,
// a service that never declared its pinned cost is rejected rather than silently
// treated as free (which would let the arbiter overcommit the card).
func TestBudgetRequiresPinnedCost(t *testing.T) {
	c := &Config{
		GPU:      GPU{VRAMBudgetMB: 10000},
		Services: map[string]Service{"x": {Image: "i", Port: 1, Evict: EvictStop}},
	}
	if err := c.validate(); err == nil {
		t.Fatal("expected an error for a budgeted service with no vram_pinned_mb")
	}
}

// TestParkedTaxDefaults checks the two defaulting rules: a stop service holds no
// parked VRAM, and a park service with no explicit tax inherits the global one.
func TestParkedTaxDefaults(t *testing.T) {
	c := &Config{
		GPU: GPU{VRAMBudgetMB: 10000, ContextTaxMB: 400},
		Services: map[string]Service{
			"stopper": {Image: "i", Port: 1, Evict: EvictStop, VRAMPinnedMB: 2000, VRAMParkedMB: 999},
			"parker":  {Image: "i", Port: 2, Evict: EvictPark, VRAMPinnedMB: 2000},
		},
	}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Services["stopper"].VRAMParkedMB; got != 0 {
		t.Errorf("stop service parked tax = %d, want 0", got)
	}
	if got := c.Services["parker"].VRAMParkedMB; got != 400 {
		t.Errorf("park service parked tax = %d, want the 400 context-tax default", got)
	}
}

// TestDisabledServiceIsDropped proves a disabled service vanishes from the map
// (so the rest of ASS never sees it) yet is recorded in DisabledServices for
// logging — and that its own fields aren't validated, so it may be incomplete.
func TestDisabledServiceIsDropped(t *testing.T) {
	c := &Config{
		Services: map[string]Service{
			"live": {Image: "i", Port: 1, Evict: EvictStop},
			// No image, no port: would fail validation if it weren't disabled.
			"off": {Disabled: true},
		},
	}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, ok := c.Services["off"]; ok {
		t.Error("disabled service should be gone from Services")
	}
	if _, ok := c.Services["live"]; !ok {
		t.Error("live service should remain")
	}
	if len(c.DisabledServices) != 1 || c.DisabledServices[0] != "off" {
		t.Errorf("DisabledServices = %v, want [off]", c.DisabledServices)
	}
}

// TestAllServicesDisabled gives a distinct, friendlier error than "none
// configured" when every service is present but switched off.
func TestAllServicesDisabled(t *testing.T) {
	c := &Config{Services: map[string]Service{"off": {Disabled: true, Image: "i", Port: 1}}}
	err := c.validate()
	if err == nil {
		t.Fatal("expected an error when every service is disabled")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error %q should mention that services are disabled", err)
	}
}

// TestRetentionDefaultsOn proves the size cap defaults ON at 15 GB when no
// [retention] table is given, so a bare config still tidies its disk — while an
// explicit max_total_mb = 0 opts back out to hoard-forever.
func TestRetentionDefaultsOn(t *testing.T) {
	base := func() *Config {
		return &Config{Services: map[string]Service{"x": {Image: "i", Port: 1, Evict: EvictStop}}}
	}

	// Absent key -> default 15000 MB, reaper active.
	c := base()
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Retention.TotalMB(); got != 15000 {
		t.Fatalf("default size cap: want 15000, got %d", got)
	}
	if !c.Retention.Active() {
		t.Fatal("retention should be active with the default size cap")
	}

	// Explicit 0 -> unlimited, reaper inert.
	c = base()
	zero := int64(0)
	c.Retention.MaxTotalMB = &zero
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c.Retention.TotalMB() != 0 || c.Retention.Active() {
		t.Fatalf("explicit 0 should disable: TotalMB=%d Active=%v", c.Retention.TotalMB(), c.Retention.Active())
	}
}
