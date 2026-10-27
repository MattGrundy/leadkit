package doctor

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// guardEnforcementCheck reports the effective enforcement posture (docs/build-tasks/
// configurable-guard-enforcement.md): Info + OK when every security guard enforces, a
// gentle Warn (never Critical) while any is still advisory. Fail-open — never a hard error.
func TestGuardEnforcementCheck_Posture(t *testing.T) {
	t.Run("all advisory (default) → gentle warn, never critical", func(t *testing.T) {
		prof := &config.Profile{} // no guards block → advisory everywhere
		c := guardEnforcementCheck(prof)
		if c.Name != "guard-enforcement" {
			t.Fatalf("check name = %q", c.Name)
		}
		if c.Severity == SeverityCritical {
			t.Errorf("advisory posture must never be Critical, got %s", c.Severity)
		}
		if c.Severity != SeverityWarn || c.OK {
			t.Errorf("all-advisory should be a soft Warn (OK=false), got severity=%s OK=%v", c.Severity, c.OK)
		}
		for _, g := range config.SecurityGuards {
			if !strings.Contains(c.Detail, g+"=advisory") {
				t.Errorf("detail should name %s=advisory, got: %s", g, c.Detail)
			}
		}
	})

	t.Run("all enforce → Info + OK", func(t *testing.T) {
		prof := &config.Profile{Guards: config.Guards{DefaultMode: config.GuardModeEnforce}}
		c := guardEnforcementCheck(prof)
		if c.Severity != SeverityInfo || !c.OK {
			t.Errorf("all-enforce should be Info + OK, got severity=%s OK=%v", c.Severity, c.OK)
		}
		for _, g := range config.SecurityGuards {
			if !strings.Contains(c.Detail, g+"=enforce") {
				t.Errorf("detail should name %s=enforce, got: %s", g, c.Detail)
			}
		}
	})

	t.Run("mixed → Warn naming the still-advisory guard", func(t *testing.T) {
		prof := &config.Profile{Guards: config.Guards{
			DefaultMode: config.GuardModeEnforce,
			OwnerGuard:  config.GuardModeSpec{Mode: config.GuardModeAdvisory},
		}}
		c := guardEnforcementCheck(prof)
		if c.Severity != SeverityWarn || c.OK {
			t.Errorf("mixed posture should be a soft Warn, got severity=%s OK=%v", c.Severity, c.OK)
		}
		if !strings.Contains(c.Detail, config.GuardOwnerGuard+"=advisory") {
			t.Errorf("detail should flag owner_guard=advisory, got: %s", c.Detail)
		}
		if !strings.Contains(c.Detail, config.GuardPushGuard+"=enforce") {
			t.Errorf("detail should show push_guard=enforce, got: %s", c.Detail)
		}
	})
}
