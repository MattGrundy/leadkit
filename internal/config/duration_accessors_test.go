package config

import (
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
)

// Every *Dur accessor on Burn resolves a duration STRING from the profile to a
// time.Duration, and every one of them must agree on the same three rules, because
// several independent consumers read each window and a disagreement between them is
// invisible: empty falls back to the package default, a valid positive duration is
// honoured, and anything else (unparseable, zero, negative) falls back rather than
// producing a window that would fire constantly or never.
//
// The zero and negative cases are the ones worth pinning. `time.ParseDuration("0s")`
// and `("-5m")` both SUCCEED, so an accessor that only checked the error would accept
// them: a 0s stale window marks every entry stale, and a negative one marks none.
func TestBurnDurationAccessors_FallbackRules(t *testing.T) {
	accessors := []struct {
		name string
		set  func(*Burn, string)
		get  func(Burn) time.Duration
		def  time.Duration
	}{
		{"workstream_stale_window", func(b *Burn, s string) { b.WorkstreamStaleWindow = s },
			Burn.WorkstreamStaleWindowDur, burn.DefaultWorkstreamStaleWindow},
		{"enforce_interval", func(b *Burn, s string) { b.EnforceInterval = s },
			Burn.EnforceIntervalDur, burn.DefaultEnforceInterval},
		{"productivity_stall_window", func(b *Burn, s string) { b.ProductivityStallWindow = s },
			Burn.ProductivityStallWindowDur, burn.DefaultProductivityStallWindow},
		{"inprogress_stall_window", func(b *Burn, s string) { b.InprogressStallWindow = s },
			Burn.InprogressStallWindowDur, burn.DefaultInprogressStallWindow},
	}

	for _, a := range accessors {
		t.Run(a.name, func(t *testing.T) {
			if a.def <= 0 {
				t.Fatalf("package default must be a positive duration, got %v", a.def)
			}

			cases := []struct {
				raw  string
				want time.Duration
				why  string
			}{
				{"", a.def, "empty means unset, so the default"},
				{"90s", 90 * time.Second, "a valid positive duration is honoured"},
				{"2h30m", 2*time.Hour + 30*time.Minute, "compound durations parse"},
				{"banana", a.def, "unparseable falls back"},
				{"0s", a.def, "zero parses but would fire on everything, so it falls back"},
				{"-5m", a.def, "negative parses but would fire on nothing, so it falls back"},
				{"15", a.def, "a bare number has no unit and does not parse"},
			}
			for _, c := range cases {
				var b Burn
				a.set(&b, c.raw)
				if got := a.get(b); got != c.want {
					t.Errorf("%q = %v, want %v (%s)", c.raw, got, c.want, c.why)
				}
			}
		})
	}
}

// A zero-value Burn (no profile at all) must still yield every default, since
// config.Defaults() is what an unconfigured adopter gets.
func TestBurnDurationAccessors_ZeroValueYieldsDefaults(t *testing.T) {
	var b Burn
	for name, got := range map[string]struct{ have, want time.Duration }{
		"workstream_stale_window":   {b.WorkstreamStaleWindowDur(), burn.DefaultWorkstreamStaleWindow},
		"enforce_interval":          {b.EnforceIntervalDur(), burn.DefaultEnforceInterval},
		"productivity_stall_window": {b.ProductivityStallWindowDur(), burn.DefaultProductivityStallWindow},
		"inprogress_stall_window":   {b.InprogressStallWindowDur(), burn.DefaultInprogressStallWindow},
	} {
		if got.have != got.want {
			t.Errorf("zero-value %s = %v, want %v", name, got.have, got.want)
		}
	}
}
