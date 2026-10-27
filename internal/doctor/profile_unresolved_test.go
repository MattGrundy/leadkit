package doctor

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// Ticket 197. An unresolved profile looks exactly like a project that chose not to gate: scope
// and authority both read as "none". doctor must say which it is.
func TestProfileCheck_AnUnresolvedProfileWarnsAndNamesTheHOMEItSearchedFrom(t *testing.T) {
	t.Setenv("HOME", "/nonexistent-home-197")
	d := config.Defaults()

	none := profileCheckAt(&d, "", t.TempDir())
	if none.OK || none.Severity != SeverityWarn {
		t.Fatalf("no profile found must be a warning, got %+v", none)
	}
	for _, want := range []string{"HOME=\"/nonexistent-home-197\"", "REFUSES", "not being found"} {
		if !strings.Contains(none.Detail, want) {
			t.Errorf("detail must contain %q, got: %s", want, none.Detail)
		}
	}

	found := profileCheckAt(&d, "/some/where/leadkit.yaml", t.TempDir())
	// (Validation warnings about the fixture's missing files are beside the point here.)
	if strings.Contains(found.Detail, "REFUSES") || strings.Contains(found.Detail, "not being found") {
		t.Errorf("a resolved profile must not claim to be missing, got %+v", found)
	}
	if !strings.Contains(found.Detail, "/some/where/leadkit.yaml") {
		t.Errorf("the check must name the profile path used, got %q", found.Detail)
	}
}
