// Package onboard is the self-serve adoption engine's canonical CONTENT: the
// adoption guide and the leadkit-adopt skill, both embedded into the binary
// using go:embed so an adopter's machine needs no repo checkout — just the
// leadkit binary. This package holds content + a small, pure state-probe formatter; it
// does not itself read a profile or run doctor (see cmd/leadkit's runOnboard,
// which reuses config.Load/install.ResolveProjectSlug/doctor.Run and hands the
// results in as plain values — the same "plain struct, caller wires the profile"
// convention every other internal package here follows, so this package stays
// decoupled from internal/config and internal/doctor).
//
// guide.md is a byte-for-byte COPY of docs/ADOPTING-LEADKIT.md — the repo's
// canonical, human-edited source of truth (go:embed cannot reach outside this
// package's own directory tree, hence the copy) — kept honest by a drift test
// that reads both and compares. SKILL.md is the canonical source for the
// leadkit-adopt skill itself: `leadkit init` installs it from here, and
// `team capture` versions the LIVE installed copy back into the repo's team_dir
// (see internal/team's SkillsDir handling) — this file is the one place its
// content is actually authored.
package onboard

import _ "embed"

// SkillName is the canonical leadkit-adopt skill's directory name — under
// ~/.claude/skills/<SkillName>/ once `leadkit init` installs it, and under
// <team_dir>/skills/<SkillName>/ once `team capture` versions it. Mirrored (as a
// literal, not an import — see internal/config's Team.SkillsDir doc comment, this
// package must not be imported by internal/config) in config.Defaults()'s default
// skills_dir.
const SkillName = "leadkit-adopt"

// GuideMD is docs/ADOPTING-LEADKIT.md's content, embedded so `leadkit onboard`
// (no flags) can print it with no repo checkout. Must stay byte-identical to the
// on-disk doc — see the drift test in onboard_test.go.
//
//go:embed guide.md
var GuideMD string

// SkillMD is the leadkit-adopt skill's SKILL.md content, embedded so `leadkit
// init` can install it with no repo checkout. This IS the skill's canonical
// source (not a copy of something else) — edit it here.
//
//go:embed SKILL.md
var SkillMD string

// CheckResult is `leadkit onboard --check`'s structured project-state probe — the
// STABLE field-name contract the leadkit-adopt skill's --json call depends on.
// Every field is a plain value the caller (cmd/leadkit) already computed by
// reusing config.Load/install.ResolveProjectSlug/doctor.Run — this type and
// BuildCheckResult below do no I/O of their own.
type CheckResult struct {
	ProfileFound  bool   `json:"profile_found"`
	ProfilePath   string `json:"profile_path,omitempty"`
	ProjectSlug   string `json:"project_slug,omitempty"`
	SlugResolved  bool   `json:"slug_resolved"`
	DoctorOK      bool   `json:"doctor_ok"`
	DoctorSummary string `json:"doctor_summary"`
	NextStep      string `json:"next_step"`
}

// BuildCheckResult assembles CheckResult from already-computed inputs. profileFound/
// profilePath come from config.Load's own return (path=="" means "no leadkit.yaml
// found" — Load's own documented signal, not an error); slug from
// install.ResolveProjectSlug (slugResolved is whether it resolved to a non-empty
// value); doctorOK/doctorSummary from doctor.Run's Report (AllCriticalPass() and a
// short human summary — the caller's job, this function only sequences the
// resulting NextStep).
func BuildCheckResult(profileFound bool, profilePath, slug string, slugResolved, doctorOK bool, doctorSummary string) CheckResult {
	r := CheckResult{
		ProfileFound: profileFound, ProfilePath: profilePath,
		ProjectSlug: slug, SlugResolved: slugResolved,
		DoctorOK: doctorOK, DoctorSummary: doctorSummary,
	}
	switch {
	case !profileFound:
		r.NextStep = "write leadkit.yaml (see `leadkit onboard` — step-by-step section), then re-run `leadkit doctor`"
	case !doctorOK:
		r.NextStep = "resolve the critical `leadkit doctor` finding(s) above (each names its fix; a project that has not yet run them needs `leadkit team scaffold --apply --confirm`, then `leadkit init --apply --confirm`), then re-run `leadkit doctor`"
	default:
		r.NextStep = "stand up one persistent owner per role with `leadkit team standup`, then enqueue the first task with the `leadkit task create --assign` command `leadkit team scaffold` printed"
	}
	return r
}

// FormatCheck renders r as `leadkit onboard --check`'s human-readable output.
func FormatCheck(r CheckResult) string {
	var profLine string
	if r.ProfileFound {
		profLine = "leadkit.yaml: found at " + r.ProfilePath
	} else {
		profLine = "leadkit.yaml: NOT FOUND — using built-in defaults"
	}
	slugLine := "project slug: (unresolved — set project_id to avoid daemon/burn-registry collisions)"
	if r.SlugResolved {
		slugLine = "project slug: " + r.ProjectSlug
	}
	doctorLine := "doctor: FAIL — see `leadkit doctor` for details"
	if r.DoctorOK {
		doctorLine = "doctor: OK (" + r.DoctorSummary + ")"
	} else if r.DoctorSummary != "" {
		doctorLine = "doctor: FAIL — " + r.DoctorSummary
	}
	return profLine + "\n" + slugLine + "\n" + doctorLine + "\nnext: " + r.NextStep + "\n"
}
