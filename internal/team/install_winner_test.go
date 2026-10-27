package team

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/starter"
)

// Ticket 103. leadkit has no runtime precedence among agent definitions; install is the only
// place it decides, and the rule is the destination's. This pins the winner when the sources
// DISAGREE, for each state a destination file can be in: asserting only that something
// shadowed something would be satisfied by any order.
func TestInstall_WhenTheDestinationAndTheEmbeddedStarterDisagree_TheInstalledFileWinsUnlessReplaced(t *testing.T) {
	cfg, _ := sourceCfg(t) // no snapshot: the embedded set is the source
	embedded, err := starter.Agents()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range embedded {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) < 3 {
		t.Fatalf("need three embedded definitions, have %v", names)
	}
	absent, customised, olderStarter := names[0], names[1], names[2]

	customisedBody := "---\nname: mine\n---\nthe team's own text, not a starter\n"
	// An older generated starter: still carries the marker, but not today's bytes.
	olderBody := string(embedded[olderStarter]) + "\nan older revision's trailing line\n"
	if !starter.IsGeneratedAgent([]byte(olderBody)) {
		t.Fatal("fixture: the older starter must still read as generated")
	}
	mustWrite(t, filepath.Join(cfg.AgentsDir, customised), customisedBody)
	mustWrite(t, filepath.Join(cfg.AgentsDir, olderStarter), olderBody)

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(cfg.AgentsDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return string(b)
	}
	apply := func(opts InstallOptions) InstallApplyResult {
		plan, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), opts)
		if err != nil {
			t.Fatal(err)
		}
		res, err := ApplyInstallPlan(plan)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Default: absent is written; a customised file AND an older generated one both stay.
	apply(InstallOptions{PreferEmbedded: true})
	if got := read(absent); got != string(embedded[absent]) {
		t.Errorf("an absent definition must be written from the embedded set")
	}
	if got := read(customised); got != customisedBody {
		t.Errorf("a customised definition must win over the embedded starter, got:\n%s", got)
	}
	if got := read(olderStarter); got != olderBody {
		t.Errorf("an older generated starter is still the installed file, and install does not replace it unasked; got:\n%s", got)
	}

	// Explicit replace: the embedded starter wins, and the loser is backed up, not lost.
	res := apply(InstallOptions{PreferEmbedded: true, ReplaceExisting: true})
	if got := read(customised); got != string(embedded[customised]) {
		t.Errorf("--replace-existing must install the embedded starter over a customised file")
	}
	bak, ok := res.BackedUpTo[customised]
	if !ok {
		t.Fatalf("the replaced customised file must be backed up, got %v", res.BackedUpTo)
	}
	if b, err := os.ReadFile(bak); err != nil || !strings.Contains(string(b), "the team's own text") {
		t.Errorf("backup %s must hold the customised text: %v", bak, err)
	}
	if got := read(olderStarter); got != string(embedded[olderStarter]) {
		t.Errorf("--replace-existing must also bring an older generated starter up to date")
	}
}
