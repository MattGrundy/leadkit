package onboard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/publish"
)

// repoRoot walks up from this test file's own source location to find the repo
// root (the dir containing go.mod) — used ONLY to read the canonical on-disk doc
// for the drift comparison below; go:embed itself can't reach outside this
// package's directory, which is exactly why guide.md is a copy in the first
// place.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found walking up)")
		}
		dir = parent
	}
}

// TestGuideMD_ByteIdenticalToCanonicalDoc is the drift guard: the embedded guide
// must never silently diverge from docs/ADOPTING-LEADKIT.md, the repo's canonical,
// human-edited source of truth.
func TestGuideMD_ByteIdenticalToCanonicalDoc(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "ADOPTING-LEADKIT.md"))
	if err != nil {
		t.Fatalf("read canonical doc: %v", err)
	}
	if GuideMD != string(canonical) {
		t.Error("embedded GuideMD has drifted from docs/ADOPTING-LEADKIT.md — copy the canonical doc back into internal/onboard/guide.md")
	}
}

// TestGuideMD_ContainsCanonicalSections asserts the required sections (spec's
// deliverable-1 list) are actually present, not just that the file is non-empty.
func TestGuideMD_ContainsCanonicalSections(t *testing.T) {
	if GuideMD == "" {
		t.Fatal("GuideMD is empty")
	}
	for _, want := range []string{
		"What leadkit is", "Prerequisites", "core model", "4 pieces to adopt",
		"What to skip", "Authority grant", "Step-by-step", "Getting unstuck",
	} {
		if !strings.Contains(GuideMD, want) {
			t.Errorf("GuideMD missing required section %q", want)
		}
	}
}

// TestSkillMD_HasFrontmatterAndNeverSelfGrantsAuthority guards the guard-adjacent
// wording: the skill must name the hard-stop explicitly, not just gesture at it.
func TestSkillMD_HasFrontmatterAndNeverSelfGrantsAuthority(t *testing.T) {
	if !strings.HasPrefix(SkillMD, "---\nname: "+SkillName) {
		t.Error("SKILL.md must start with frontmatter naming the skill")
	}
	for _, want := range []string{"HARD STOP", "authority", "Matt", "never self-grant", "relayed"} {
		if !strings.Contains(strings.ToLower(SkillMD), strings.ToLower(want)) {
			t.Errorf("SKILL.md missing required guard wording %q", want)
		}
	}
}

func TestBuildCheckResult_NoProfileFoundPointsAtWritingIt(t *testing.T) {
	r := BuildCheckResult(false, "", "", false, false, "")
	if r.ProfileFound {
		t.Error("want ProfileFound=false")
	}
	if !strings.Contains(r.NextStep, "leadkit.yaml") {
		t.Errorf("NextStep = %q, want it to point at writing leadkit.yaml", r.NextStep)
	}
}

func TestBuildCheckResult_DoctorFailingPointsAtDoctorAndTheDocumentedOrder(t *testing.T) {
	r := BuildCheckResult(true, "/p/leadkit.yaml", "myproj", true, false, "3 critical checks failing")
	if r.NextStep == "" || !strings.Contains(r.NextStep, "doctor") {
		t.Errorf("NextStep = %q, want it to point at resolving doctor findings", r.NextStep)
	}
	s, i := strings.Index(r.NextStep, "team scaffold"), strings.Index(r.NextStep, "init --apply")
	if s < 0 || i < 0 || s > i {
		t.Errorf("NextStep = %q, want it to name scaffold and then init, the documented order", r.NextStep)
	}
}

// Doctor green means the install steps are done. What is left is the owners and the first task, and
// pointing back at scaffold would tell an adopter to repeat a step they just completed.
func TestBuildCheckResult_AllGreenPointsAtTheOwnersThenTheFirstTask(t *testing.T) {
	r := BuildCheckResult(true, "/p/leadkit.yaml", "myproj", true, true, "all critical checks pass")
	for _, want := range []string{"team standup", "task create --assign"} {
		if !strings.Contains(r.NextStep, want) {
			t.Errorf("NextStep = %q, want it to name %q", r.NextStep, want)
		}
	}
	if strings.Index(r.NextStep, "team standup") > strings.Index(r.NextStep, "task create") {
		t.Errorf("NextStep = %q, want the owners stood up before the first task", r.NextStep)
	}
	if strings.Contains(r.NextStep, "scaffold --apply") {
		t.Errorf("NextStep = %q, must not send a finished adopter back to scaffold", r.NextStep)
	}
}

func TestFormatCheck_RendersEveryField(t *testing.T) {
	r := BuildCheckResult(true, "/p/leadkit.yaml", "myproj", true, true, "all critical checks pass")
	out := FormatCheck(r)
	for _, want := range []string{"/p/leadkit.yaml", "myproj", "OK", "team standup"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatCheck output = %q, want it to contain %q", out, want)
		}
	}
}

// init installs the waker and sweep only for a queue that exists, and scaffold creates the queue, so
// every adopter-facing statement of the order must put scaffold first. The walk checks the guide; this
// checks the skill the lead's agent reads, alongside the guide text.
func TestAdopterFacingOrder_ScaffoldComesBeforeInit(t *testing.T) {
	const scaffold, initCmd = "team scaffold --apply --confirm", "init --apply --confirm"
	for name, text := range map[string]string{"SKILL.md": SkillMD, "guide.md": GuideMD} {
		flat := strings.Join(strings.Fields(text), " ")
		s, i := strings.Index(flat, scaffold), strings.Index(flat, initCmd)
		if s < 0 || i < 0 {
			t.Errorf("%s must name both commands (scaffold at %d, init at %d)", name, s, i)
			continue
		}
		if s > i {
			t.Errorf("%s runs init before scaffold; init skips the daemons when the queue does not exist yet", name)
		}
	}
}

// The default review gate has two layers, and nothing an adopter is shown may send them to stand up a
// third. The same adopter-facing text exists in several files (the skill, the embedded guide `leadkit
// onboard` prints, the canonical doc it is copied from, and the operating kit), so every copy is
// scanned: a check scoped to the file a fix touched cannot find the copy it did not.
func TestAdopterFacingText_DoesNotTeachTheRetiredThirdLayer(t *testing.T) {
	root := repoRoot(t)
	manifest, err := publish.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{
		"SKILL.md (embedded)":          SkillMD,
		"guide.md (embedded, printed)": GuideMD,
	}
	for _, rel := range []string{"docs/ADOPTING-LEADKIT.md", "docs/PORTABLE-OPERATING-KIT.md"} {
		text, skipped, err := readAdopterDoc(root, rel, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if skipped {
			t.Run("skipped "+rel, func(t *testing.T) {
				t.Skipf("NOT CHECKED: %s is absent from this tree and listed in %s, so the retired-third-layer scan did not cover it", rel, publish.DefaultManifest)
			})
			continue
		}
		texts[rel] = text
	}
	for name, text := range texts {
		if strings.Contains(text, "outcome-verifier") {
			t.Errorf("%s names outcome-verifier, a retired layer no lane serves", name)
		}
		for _, retired := range []string{"critique → outcome", "critique -> outcome", "critique + outcome-verifier"} {
			if strings.Contains(text, retired) {
				t.Errorf("%s still teaches the retired third layer: %q", name, retired)
			}
		}
	}
}

// readAdopterDoc reads rel under root. A file that does not exist is skipped only when manifest
// excludes it, since `leadkit publish prepare` removes exactly those docs; absent and unlisted is a
// deleted or renamed doc, and any other read failure is an error, so neither can pass as a skip.
func readAdopterDoc(root, rel string, manifest []string) (text string, skipped bool, err error) {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		return string(b), false, nil
	case errors.Is(err, fs.ErrNotExist) && publish.Excluded(manifest, rel):
		return "", true, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", false, fmt.Errorf("%s does not exist and %s does not exclude it: the doc was deleted or renamed, so the scan would silently stop covering it", rel, publish.DefaultManifest)
	default:
		return "", false, fmt.Errorf("read %s: %w", rel, err)
	}
}

// The skip/fail split of readAdopterDoc. The unreadable case lists its path in the manifest on
// purpose: only then does widening the not-exist test to any error turn the failure into a skip.
func TestReadAdopterDoc_SkipsOnlyWhatTheManifestExcludes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "unreadable.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []string{"gone.md", "unreadable.md"}

	if text, skipped, err := readAdopterDoc(dir, "present.md", manifest); err != nil || skipped || text != "hello" {
		t.Errorf("present: got %q, skipped=%v, err=%v; want its text, not skipped", text, skipped, err)
	}
	if _, skipped, err := readAdopterDoc(dir, "gone.md", manifest); err != nil || !skipped {
		t.Errorf("absent and listed: skipped=%v, err=%v; want a skip", skipped, err)
	}
	if _, skipped, err := readAdopterDoc(dir, "renamed.md", manifest); err == nil || skipped {
		t.Errorf("absent and unlisted: skipped=%v, err=%v; want an error", skipped, err)
	}
	if _, skipped, err := readAdopterDoc(dir, "unreadable.md", manifest); err == nil || skipped {
		t.Errorf("present but unreadable and listed: skipped=%v, err=%v; want an error", skipped, err)
	}
}
