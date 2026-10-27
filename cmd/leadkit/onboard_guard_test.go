package main

// End-to-end tests for adoption-onboard's two cmd/leadkit surfaces: `leadkit
// onboard` (the engine) and the skill install `leadkit init` performs (guarded the
// same way hooks/daemons are). Reuses runCLI (mandatory HOME/cwd/LEADKIT_PROFILE
// sandboxing — see apply_guard_test.go's package doc) so these can never touch a
// real ~/.claude.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_Onboard_PrintsCanonicalGuide(t *testing.T) {
	bin := testBinary(t)
	out, code := runCLI(t, bin, nil, "onboard")
	if code != 0 {
		t.Fatalf("`onboard` should exit 0, got %d. output:\n%s", code, out)
	}
	for _, want := range []string{"What leadkit is", "queue mode is the substrate", "Authority grant"} {
		if !strings.Contains(out, want) {
			t.Errorf("`onboard` output missing %q:\n%s", want, out)
		}
	}
}

func TestCLI_OnboardCheck_JSONSchemaNoProfile(t *testing.T) {
	bin := testBinary(t)
	out, code := runCLI(t, bin, nil, "onboard", "--check", "--json")
	if code != 0 {
		t.Fatalf("`onboard --check --json` should exit 0, got %d. output:\n%s", code, out)
	}
	// runCLI merges stdout+stderr — a sandboxed, nonexistent LEADKIT_PROFILE makes
	// config.Load() itself log a (non-fatal, same as `doctor`'s own precedent)
	// warning to stderr before the JSON line; take the last non-empty line, the
	// same way a real caller parsing this output would.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	jsonLine := lines[len(lines)-1]

	var result struct {
		ProfileFound bool   `json:"profile_found"`
		DoctorOK     bool   `json:"doctor_ok"`
		NextStep     string `json:"next_step"`
	}
	if err := json.Unmarshal([]byte(jsonLine), &result); err != nil {
		t.Fatalf("last output line is not valid JSON: %v\n%s", err, out)
	}
	if result.ProfileFound {
		t.Error("want profile_found=false — the sandbox has no leadkit.yaml")
	}
	if !strings.Contains(result.NextStep, "leadkit.yaml") {
		t.Errorf("next_step = %q, want it to point at writing leadkit.yaml", result.NextStep)
	}
}

// TestCLI_Init_SkillDryRunThenApplyThenIdempotentAndNonClobbering covers the
// spec's explicit skill-install test list: dry-run lists what would be created;
// --apply --confirm creates it; re-init is idempotent; a sibling non-leadkit skill
// is left untouched.
func TestCLI_Init_SkillDryRunThenApplyThenIdempotentAndNonClobbering(t *testing.T) {
	bin := testBinary(t)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	launchAgentsDir := t.TempDir()
	skillsDir := t.TempDir()

	// A pre-existing, unrelated skill — must never be touched by anything below.
	otherSkillFile := filepath.Join(skillsDir, "some-other-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(otherSkillFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherSkillFile, []byte("not leadkit's\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dryOut, code := runCLI(t, bin, nil,
		"init", "--settings", settingsPath, "--launch-agents-dir", launchAgentsDir, "--skills-dir", skillsDir)
	if code != 0 {
		t.Fatalf("dry-run `init` should exit 0, got %d. output:\n%s", code, dryOut)
	}
	if !strings.Contains(dryOut, "skill") || !strings.Contains(dryOut, "leadkit-adopt") {
		t.Errorf("dry-run output should list the skill it would install:\n%s", dryOut)
	}
	skillDest := filepath.Join(skillsDir, "leadkit-adopt", "SKILL.md")
	if _, err := os.Stat(skillDest); !os.IsNotExist(err) {
		t.Error("dry-run must not have created the skill file")
	}

	outApplied, codeApplied := runCLI(t, bin, nil,
		"init", "--apply", "--confirm", "--settings", settingsPath,
		"--launch-agents-dir", launchAgentsDir, "--skills-dir", skillsDir, "--skip-launchctl")
	if codeApplied != 0 {
		t.Fatalf("`init --apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	written, err := os.ReadFile(skillDest)
	if err != nil {
		t.Fatalf("skill file not written: %v", err)
	}
	if !strings.Contains(string(written), "name: leadkit-adopt") {
		t.Errorf("written SKILL.md missing its own frontmatter:\n%s", written)
	}

	// Sibling skill must be untouched.
	otherAfter, err := os.ReadFile(otherSkillFile)
	if err != nil || string(otherAfter) != "not leadkit's\n" {
		t.Errorf("sibling skill changed: %q, %v", otherAfter, err)
	}

	// Re-init: idempotent — nothing left to apply for the skill.
	reOut, reCode := runCLI(t, bin, nil,
		"init", "--apply", "--confirm", "--settings", settingsPath,
		"--launch-agents-dir", launchAgentsDir, "--skills-dir", skillsDir, "--skip-launchctl")
	if reCode != 0 {
		t.Fatalf("re-`init --apply --confirm` should exit 0, got %d. output:\n%s", reCode, reOut)
	}
	if !strings.Contains(reOut, "nothing to apply") {
		t.Errorf("re-init should report the skill as already matching, got:\n%s", reOut)
	}
}
