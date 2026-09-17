package config

import "testing"

// TestWebEnabledDefault: an absent [web] table means the console is ON, and
// `enabled = false` turns it off. (A plain bool would default to off, hence the
// *bool — this guards that choice.)
func TestWebEnabledDefault(t *testing.T) {
	f := false
	tr := true
	cases := []struct {
		name string
		web  Web
		want bool
	}{
		{"absent table", Web{}, true},
		{"enabled=false", Web{Enabled: &f}, false},
		{"enabled=true", Web{Enabled: &tr}, true},
	}
	for _, tc := range cases {
		c := &Config{Web: tc.web}
		if got := c.WebEnabled(); got != tc.want {
			t.Errorf("%s: WebEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
