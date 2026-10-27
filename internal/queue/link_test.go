package queue

import (
	"os"
	"path/filepath"
	"testing"
)

// aliasedTree builds real/ plus alias -> real/ (the shape of macOS /var -> /private/var) and
// returns both spellings of the root.
func aliasedTree(t *testing.T) (realRoot, aliasRoot string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realRoot = filepath.Join(base, "deep", "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot = filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	return realRoot, aliasRoot
}

// The incident: the queue was spelled one way and the store the other, filepath.Rel produced a
// target of the wrong depth, and the link dangled while looking correct. Every row here dangles
// under a plain filepath.Rel of the two spellings.
func TestRelativeLinkTarget_ResolvesWhicheverSpellingEachSideUses(t *testing.T) {
	realRoot, alias := aliasedTree(t)
	lane := filepath.Join(realRoot, "queue", "code-review", "pending")
	if err := os.MkdirAll(lane, 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(realRoot, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	ticket := filepath.Join(store, "1-x.md")
	if err := os.WriteFile(ticket, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string][2]string{
		"link dir via alias, store via real": {filepath.Join(alias, "queue", "code-review", "pending"), ticket},
		"link dir via real, store via alias": {lane, filepath.Join(alias, "tasks", "1-x.md")},
		"both via alias":                     {filepath.Join(alias, "queue", "code-review", "pending"), filepath.Join(alias, "tasks", "1-x.md")},
		"both via real":                      {lane, ticket},
	} {
		t.Run(name, func(t *testing.T) {
			target := RelativeLinkTarget(c[0], c[1])
			link := filepath.Join(lane, "1-x.md")
			_ = os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(link); err != nil {
				t.Fatalf("link -> %q dangles: %v", target, err)
			}
			if !LinkResolves(c[0], target) {
				t.Errorf("LinkResolves disagrees with the filesystem for %q", target)
			}
		})
	}
}

func TestRelativeLinkTarget_FallsBackToAnAbsoluteTargetWhenItCannotResolve(t *testing.T) {
	target := RelativeLinkTarget(filepath.Join(t.TempDir(), "no", "such", "dir"), "/some/tasks/1-x.md")
	if !filepath.IsAbs(target) {
		t.Errorf("an unresolvable link dir must give an absolute target, got %q", target)
	}
}

func TestLinkResolves_IsFalseForADanglingTarget(t *testing.T) {
	dir := t.TempDir()
	if LinkResolves(dir, "../../nowhere/1-x.md") {
		t.Error("a target that does not exist must not resolve")
	}
}

// repair re-points a mispointed link using the same helper. The discriminating combination is a
// link dir spelled one way and the store the other: both through the alias is the one pairing a
// plain filepath.Rel gets right, so it cannot tell the helper from Rel.
func TestPlanRepoint_ResolvesWhenLinkDirAndStoreUseDifferentSpellings(t *testing.T) {
	realRoot, alias := aliasedTree(t)
	lane := filepath.Join(realRoot, "queue", "developer", "pending")
	store := filepath.Join(realRoot, "tasks")
	for _, d := range []string{lane, store} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store, "1-x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"link dir via alias, store via real": {filepath.Join(alias, "queue", "developer", "pending", "1-x.md"), store},
		"link dir via real, store via alias": {filepath.Join(lane, "1-x.md"), filepath.Join(alias, "tasks")},
	} {
		t.Run(name, func(t *testing.T) {
			rep := planRepoint(DanglingLink{Path: c[0], Name: "1-x.md", Target: "../../../wrong/1-x.md"}, c[1], "1-x.md")
			if !LinkResolves(filepath.Dir(c[0]), rep.NewTarget) {
				t.Errorf("the planned re-point %q does not resolve", rep.NewTarget)
			}
			// and it really does, on disk
			link := filepath.Join(lane, "probe.md")
			_ = os.Remove(link)
			if err := os.Symlink(rep.NewTarget, link); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(link); err != nil {
				t.Errorf("link -> %q dangles: %v", rep.NewTarget, err)
			}
			_ = os.Remove(link)
		})
	}
}

// A relative target whose ".." crosses the alias boundary is where lexical and physical resolution
// differ: the kernel resolves it from the REAL directory holding the link. LinkResolves must agree
// with the kernel, so it has to resolve the link dir; a lexical join would pass these both wrongly.
func TestLinkResolves_FollowsThePhysicalDirectoryNotTheAliasSpelling(t *testing.T) {
	realRoot, alias := aliasedTree(t) // alias -> <base>/deep/real
	lane := filepath.Join(realRoot, "queue", "lane")
	if err := os.MkdirAll(lane, 0o755); err != nil {
		t.Fatal(err)
	}
	viaAlias := filepath.Join(alias, "queue", "lane")
	base := filepath.Dir(filepath.Dir(realRoot)) // the dir holding deep/ and alias

	// physically, ../../../x from <base>/deep/real/queue/lane is <base>/deep/x
	deep := filepath.Join(base, "deep", "x")
	if err := os.WriteFile(deep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !LinkResolves(viaAlias, "../../../x") {
		t.Error("the file physically reachable from the link's real directory must resolve (a lexical join would look in <base>/x)")
	}
	if err := os.Remove(deep); err != nil {
		t.Fatal(err)
	}
	// lexically (via the alias spelling) the same target would be <base>/x
	if err := os.WriteFile(filepath.Join(base, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if LinkResolves(viaAlias, "../../../x") {
		t.Error("a file only a LEXICAL join would find must not resolve: the kernel will not find it")
	}
}

func TestLinkResolves_AbsoluteTargets(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "t.md")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !LinkResolves(dir, f) {
		t.Error("an existing absolute target resolves")
	}
	if LinkResolves(dir, filepath.Join(dir, "missing.md")) {
		t.Error("a missing absolute target does not")
	}
}

// "target need not exist yet": its directory must, and the result must resolve once it does.
func TestRelativeLinkTarget_ToleratesAMissingLeafButNotAMissingParent(t *testing.T) {
	realRoot, alias := aliasedTree(t)
	lane := filepath.Join(realRoot, "queue", "lane")
	store := filepath.Join(realRoot, "tasks")
	for _, d := range []string{lane, store} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := RelativeLinkTarget(filepath.Join(alias, "queue", "lane"), filepath.Join(alias, "tasks", "later.md"))
	if filepath.IsAbs(target) {
		t.Fatalf("a missing leaf under an existing dir must still yield a relative target, got %q", target)
	}
	if err := os.WriteFile(filepath.Join(store, "later.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(lane, "later.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("once the file exists the link must resolve: %v", err)
	}
	if got := RelativeLinkTarget(lane, filepath.Join(alias, "no-such-dir", "x.md")); !filepath.IsAbs(got) {
		t.Errorf("a missing PARENT must fall back to an absolute target, got %q", got)
	}
}
