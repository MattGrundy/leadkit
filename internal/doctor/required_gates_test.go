package doctor

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// doctor reports a grant that requires a layer the project does not have, because no record can ever
// satisfy it and the first sign would otherwise be a PR that cannot be opened.
func TestProfileCheck_ReportsARequiredGateNamingNoLayer(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{"acme/widgets": {RequiredGates: []string{"code-review", "security"}}}
	c := profileCheckAt(prof, "/p/leadkit.yaml", t.TempDir())
	if c.OK || c.Severity != SeverityWarn || !strings.Contains(c.Detail, `authority.acme/widgets.required_gates: "security" is not a review layer`) {
		t.Fatalf("check = %+v, want a warning naming the grant and the layer", c)
	}

	prof.Roster = []config.RosterSpec{{Type: "security", Count: 1, ReviewLayer: true}}
	if c := profileCheckAt(prof, "/p/leadkit.yaml", t.TempDir()); strings.Contains(c.Detail, "required_gates") {
		t.Errorf("declaring the role a review layer makes the grant valid: %+v", c)
	}
}
