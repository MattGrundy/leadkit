package config

import "testing"

// EffectiveGuardMode is the single mode resolver every guard + doctor shares: a guard's
// own mode > default_mode > advisory, and anything not exactly "enforce" is advisory (the
// safe, backward-compatible default). See docs/build-tasks/configurable-guard-enforcement.md.
func TestEffectiveGuardMode(t *testing.T) {
	cases := []struct {
		name   string
		guards Guards
		guard  string
		want   string
	}{
		{"unset → advisory (backward-compat)", Guards{}, GuardPushGuard, GuardModeAdvisory},
		{"default_mode enforce applies to all", Guards{DefaultMode: GuardModeEnforce}, GuardOwnerGuard, GuardModeEnforce},
		{"per-guard override wins over default", Guards{DefaultMode: GuardModeAdvisory, PushGate: GuardModeSpec{Mode: GuardModeEnforce}}, GuardPushGate, GuardModeEnforce},
		{"per-guard advisory overrides an enforce default", Guards{DefaultMode: GuardModeEnforce, PushGuard: GuardModeSpec{Mode: GuardModeAdvisory}}, GuardPushGuard, GuardModeAdvisory},
		{"unset guard inherits enforce default", Guards{DefaultMode: GuardModeEnforce, PushGuard: GuardModeSpec{Mode: GuardModeAdvisory}}, GuardOwnerGuard, GuardModeEnforce},
		{"unknown mode string → advisory (never a silent enforce)", Guards{DefaultMode: "bogus"}, GuardPushGuard, GuardModeAdvisory},
		{"unknown guard name → default_mode fallback", Guards{DefaultMode: GuardModeEnforce}, "not_a_guard", GuardModeEnforce},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Profile{Guards: tc.guards}
			if got := p.EffectiveGuardMode(tc.guard); got != tc.want {
				t.Errorf("EffectiveGuardMode(%q) = %q, want %q", tc.guard, got, tc.want)
			}
		})
	}
}

func TestEffectiveGuardMode_NilProfile(t *testing.T) {
	var p *Profile
	if got := p.EffectiveGuardMode(GuardPushGuard); got != GuardModeAdvisory {
		t.Errorf("nil profile EffectiveGuardMode = %q, want advisory", got)
	}
}

// Defaults() must ship advisory so an adopter who never writes a guards block keeps
// today's fail-open posture (the opt-in guarantee).
func TestDefaults_GuardsAdvisory(t *testing.T) {
	d := Defaults()
	for _, g := range SecurityGuards {
		if got := d.EffectiveGuardMode(g); got != GuardModeAdvisory {
			t.Errorf("Defaults() %s = %q, want advisory (enforcement is opt-in)", g, got)
		}
	}
}
