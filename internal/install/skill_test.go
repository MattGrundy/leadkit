package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComputeSkillPlan_FreshDirPlansTheFile(t *testing.T) {
	skillsDir := t.TempDir()
	files := map[string][]byte{"SKILL.md": []byte("---\nname: leadkit-adopt\n---\nbody\n")}

	plan := ComputeSkillPlan(skillsDir, "leadkit-adopt", files)
	if !plan.AnyChanged() {
		t.Fatal("want AnyChanged=true for a fresh skills dir")
	}
	if len(plan.Files) != 1 || !plan.Files[0].Changed {
		t.Fatalf("Files = %+v, want one Changed file", plan.Files)
	}
	wantDest := filepath.Join(skillsDir, "leadkit-adopt", "SKILL.md")
	if plan.Files[0].DestPath != wantDest {
		t.Errorf("DestPath = %q, want %q", plan.Files[0].DestPath, wantDest)
	}
	if _, err := os.Stat(wantDest); err == nil {
		t.Error("ComputeSkillPlan must not write anything")
	}
}

func TestApplySkillPlan_WritesFileThenIsIdempotent(t *testing.T) {
	skillsDir := t.TempDir()
	content := []byte("---\nname: leadkit-adopt\n---\nbody\n")
	files := map[string][]byte{"SKILL.md": content}

	plan := ComputeSkillPlan(skillsDir, "leadkit-adopt", files)
	res, err := ApplySkillPlan(plan)
	if err != nil {
		t.Fatalf("ApplySkillPlan: %v", err)
	}
	if len(res.WrittenFiles) != 1 {
		t.Fatalf("WrittenFiles = %v, want 1", res.WrittenFiles)
	}
	got, err := os.ReadFile(filepath.Join(skillsDir, "leadkit-adopt", "SKILL.md"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("written content = %q, %v, want %q", got, err, content)
	}

	// Re-computing against the now-installed dir must show nothing left to do.
	plan2 := ComputeSkillPlan(skillsDir, "leadkit-adopt", files)
	if plan2.AnyChanged() {
		t.Errorf("re-run should be a no-op, got: %+v", plan2.Files)
	}
}

func TestApplySkillPlan_UpdateBacksUpDifferingContent(t *testing.T) {
	skillsDir := t.TempDir()
	dest := filepath.Join(skillsDir, "leadkit-adopt", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	newContent := []byte("new content\n")
	plan := ComputeSkillPlan(skillsDir, "leadkit-adopt", map[string][]byte{"SKILL.md": newContent})
	if !plan.AnyChanged() {
		t.Fatal("want a change — content differs from what's on disk")
	}
	res, err := ApplySkillPlan(plan)
	if err != nil {
		t.Fatalf("ApplySkillPlan: %v", err)
	}
	if _, ok := res.BackedUpTo["SKILL.md"]; !ok {
		t.Error("want a backup recorded for the differing existing file")
	}
	if _, err := os.Stat(res.BackedUpTo["SKILL.md"]); err != nil {
		t.Errorf("backup file should exist: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(newContent) {
		t.Fatalf("dest content = %q, %v, want %q", got, err, newContent)
	}
}

// TestComputeSkillPlan_NeverTouchesASiblingSkillDir is the "non-clobbering" test
// the spec calls out: a pre-existing, unrelated skill directory next to
// leadkit-adopt must be left completely alone.
func TestComputeSkillPlan_NeverTouchesASiblingSkillDir(t *testing.T) {
	skillsDir := t.TempDir()
	otherDir := filepath.Join(skillsDir, "some-other-skill")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(otherDir, "SKILL.md")
	if err := os.WriteFile(otherFile, []byte("not leadkit's\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := ComputeSkillPlan(skillsDir, "leadkit-adopt", map[string][]byte{"SKILL.md": []byte("leadkit content\n")})
	if _, err := ApplySkillPlan(plan); err != nil {
		t.Fatalf("ApplySkillPlan: %v", err)
	}

	got, err := os.ReadFile(otherFile)
	if err != nil || string(got) != "not leadkit's\n" {
		t.Errorf("sibling skill file changed: %q, %v", got, err)
	}
}
