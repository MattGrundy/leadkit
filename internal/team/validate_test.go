package team

import (
	"path/filepath"
	"strings"
	"testing"
)

func buildValidateFixture(t *testing.T) (teamDir, refDir string) {
	t.Helper()
	root := t.TempDir()
	teamDir = filepath.Join(root, "team-agents")
	refDir = filepath.Join(root, "reference")

	// Well-formed, well-scoped, well-described — a clean baseline.
	mustWrite(t, filepath.Join(teamDir, "ci-infra-agent.md"),
		"---\nname: ci-infra-agent\ndescription: \"Use this agent when provisioning CI runners, container registries, or the Argo controller — owns CI runner infrastructure and secrets management.\"\ntools: Read, Grep, Bash\n---\nbody\n")

	// Missing description entirely.
	mustWrite(t, filepath.Join(teamDir, "no-desc-agent.md"),
		"---\nname: no-desc-agent\ntools: Read, Grep\n---\nbody\n")

	// Broad tools grant (explicit "All tools").
	mustWrite(t, filepath.Join(teamDir, "broad-agent.md"),
		"---\nname: broad-agent\ndescription: \"Use this agent when you need broad unscoped access for emergency triage across the whole system.\"\ntools: All tools\n---\nbody\n")

	// No tools field at all (implicit unrestricted).
	mustWrite(t, filepath.Join(teamDir, "lead.md"),
		"---\nname: lead\ndescription: \"Use this agent to orchestrate the team, plan work, spawn sub-agents and batch commits into per-repo PRs.\"\n---\nbody\n")

	// Bare-label description (short, no when-to-use signal).
	mustWrite(t, filepath.Join(teamDir, "bare-label-agent.md"),
		"---\nname: bare-label-agent\ndescription: \"Devops agent\"\ntools: Bash\n---\nbody\n")

	// Reference: one role that clearly overlaps ci-infra-agent (should NOT be a gap).
	mustWrite(t, filepath.Join(refDir, "categories", "03-infrastructure", "ci-infra-specialist.md"),
		"---\nname: ci-infra-specialist\ndescription: \"Use this agent when provisioning CI runners, container registries, or Argo — infrastructure and secrets.\"\ntools: Read, Bash\n---\nbody\n")

	// Reference: a role with no team equivalent at all (should surface as a gap).
	mustWrite(t, filepath.Join(refDir, "categories", "04-quality-security", "security-auditor.md"),
		"---\nname: security-auditor\ndescription: \"Use this agent to perform penetration testing, vulnerability scanning, and compliance audits of the codebase.\"\ntools: Read, Bash\n---\nbody\n")

	return teamDir, refDir
}

func TestValidate_MissingDescriptionIsFlagged(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Hygiene {
		if f.Def == "no-desc-agent.md" && strings.Contains(f.Detail, "description") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a hygiene finding for no-desc-agent.md's missing description, got %+v", res.Hygiene)
	}
}

func TestValidate_AllToolsIsNotedAsScoping(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatal(err)
	}
	var broad, noTools bool
	for _, f := range res.ToolScoping {
		if f.Def == "broad-agent.md" {
			broad = true
		}
		if f.Def == "lead.md" {
			noTools = true
		}
	}
	if !broad {
		t.Errorf("want broad-agent.md ('All tools') noted in ToolScoping, got %+v", res.ToolScoping)
	}
	if !noTools {
		t.Errorf("want lead.md (no tools field at all) noted in ToolScoping, got %+v", res.ToolScoping)
	}
	// A tightly-scoped def should NOT appear.
	for _, f := range res.ToolScoping {
		if f.Def == "ci-infra-agent.md" {
			t.Errorf("ci-infra-agent.md is properly scoped (Read, Grep, Bash) — should not be flagged: %+v", f)
		}
	}
}

func TestValidate_ReferenceOnlyRoleIsACoverageGap(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range res.CoverageGaps {
		if g.Category == "04-quality-security" {
			found = true
			hasSecurityAuditor := false
			for _, ex := range g.Examples {
				if ex == "security-auditor" {
					hasSecurityAuditor = true
				}
			}
			if !hasSecurityAuditor {
				t.Errorf("category gap examples = %v, want security-auditor listed", g.Examples)
			}
		}
	}
	if !found {
		t.Errorf("want a coverage gap for the 04-quality-security category (security-auditor has no team match), got %+v", res.CoverageGaps)
	}
	// The infrastructure category, where ci-infra-agent DOES match, should not
	// appear as a gap category (its one role is matched).
	for _, g := range res.CoverageGaps {
		if g.Category == "03-infrastructure" {
			t.Errorf("03-infrastructure should not be a gap — ci-infra-agent matches ci-infra-specialist: %+v", g)
		}
	}
}

func TestValidate_TeamOnlyDomainAgentIsInformationalNotFlagged(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.CoverageExtra {
		if f.Def == "lead.md" {
			found = true
			if f.Category != CategoryCoverageExtra {
				t.Errorf("lead.md's coverage-extra finding has Category=%q, want %q", f.Category, CategoryCoverageExtra)
			}
		}
	}
	if !found {
		t.Errorf("want lead.md listed as a team-only (informational) role — no reference orchestrator-lead def exists in this fixture, got %+v", res.CoverageExtra)
	}
	// Never appears as a "flagged" hygiene/quality finding just for being team-only.
	for _, f := range res.Hygiene {
		if f.Def == "lead.md" {
			t.Errorf("lead.md should not have a hygiene finding from coverage matching: %+v", f)
		}
	}
}

func TestValidate_BareLabelDescriptionGetsQualityNudge(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Quality {
		if f.Def == "bare-label-agent.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("want a quality nudge for bare-label-agent.md's thin description, got %+v", res.Quality)
	}
	// The well-written ci-infra-agent description must NOT be nudged.
	for _, f := range res.Quality {
		if f.Def == "ci-infra-agent.md" {
			t.Errorf("ci-infra-agent.md has a specific when-to-use description — should not get a quality nudge: %+v", f)
		}
	}
}

func TestValidate_MalformedFrontmatterIsFlaggedNotCrashed(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	mustWrite(t, filepath.Join(teamDir, "broken.md"), "---\nname: broken\ntools: [unterminated\n---\nbody\n")

	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatalf("Validate must not error on a malformed def, got: %v", err)
	}
	found := false
	for _, f := range res.Hygiene {
		if f.Def == "broken.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("want broken.md flagged as malformed YAML, got %+v", res.Hygiene)
	}
}

func TestValidate_EmptyReferenceDirStillRunsHygieneChecks(t *testing.T) {
	teamDir, _ := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, filepath.Join(t.TempDir(), "no-reference-fetched-yet"))
	if err != nil {
		t.Fatalf("want no error with an absent reference dir, got: %v", err)
	}
	if len(res.Hygiene) == 0 {
		t.Error("hygiene checks should still run with no reference fetched")
	}
	if len(res.CoverageGaps) != 0 {
		t.Errorf("no reference at all means no gaps to report, got %+v", res.CoverageGaps)
	}
}

func TestFormatValidateResult_IncludesSummaryLine(t *testing.T) {
	teamDir, refDir := buildValidateFixture(t)
	res, err := Validate(teamDir, teamDir, refDir)
	if err != nil {
		t.Fatal(err)
	}
	out := FormatValidateResult(res)
	if !strings.Contains(out, "summary:") {
		t.Errorf("want a summary line in the formatted report:\n%s", out)
	}
}
