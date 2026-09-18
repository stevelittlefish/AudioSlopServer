package config

import "testing"

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
