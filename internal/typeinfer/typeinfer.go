// Package typeinfer is the SINGLE shared rule for turning a task's messy real-world
// owner/lane data into a canonical agent `type` — used by BOTH
// internal/orchestrate's planner (routes on it) and `leadkit queue backfill-type`
// (writes it). Sharing one implementation is the whole point: the planner and the
// migration must agree on exactly what "ci-infra" and "ci-infra-agent" resolve to,
// or the backfill wouldn't actually fix what the planner sees.
package typeinfer

import (
	"os"
	"strings"

	"mgpp.dev/leadkit/internal/starter"
)

// LoadCanonical reads every *.md basename under agentsDir (team/agents/) as one
// canonical type name — deliberately NOT hardcoded (the spec's own requirement):
// whatever role defs exist in the repo ARE the canonical set, so adding a new agent
// def is all it takes to teach both the planner and the backfill about a new type.
// A missing/unreadable agentsDir yields an empty set, not an error (both callers
// already treat "can't resolve" as "manual", never a crash).
func LoadCanonical(agentsDir string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		out[normalize(strings.TrimSuffix(name, ".md"))] = true
	}
	return out
}

// LoadCanonicalOrEmbedded is LoadCanonical with the binary's embedded agent definitions as
// a fallback when agentsDir yields nothing. A project's OWN definitions always win when it
// has any.
//
// This is deliberately NOT folded into LoadCanonical, whose empty-set-on-missing contract is
// load-bearing in two different directions. Callers that VALIDATE a caller-supplied type
// (taskcreate) fail closed on an empty set, rejecting every type, so an absent team dir reads
// as "bad type" rather than "no types configured" — those want the fallback. Callers that
// INFER a type from surrounding signals (hierarchy, orchestrate, backfill) simply decline to
// infer on an empty set, which is correct and conservative; giving them a populated set would
// make them start re-typing tasks from names the project never declared.
//
// Use this only on the validation path. The distinction exists because untracking a captured
// snapshot is otherwise a silent regression for `task create`.
func LoadCanonicalOrEmbedded(agentsDir string) map[string]bool {
	out := LoadCanonical(agentsDir)
	if len(out) > 0 {
		return out
	}
	agents, err := starter.Agents()
	if err != nil {
		return out
	}
	for name := range agents {
		out[normalize(strings.TrimSuffix(name, ".md"))] = true
	}
	return out
}

// normalize lowercases and folds "_"/" " to "-" so "CI Infra", "ci_infra", and
// "ci-infra" all compare equal — matching is case/separator-insensitive per spec.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// IsCanonical normalizes raw (case/separator-insensitive) and reports whether it is
// EXACTLY a member of canonical — no "-agent" suffix guessing (that's Match, for the
// fuzzier owner/lane inference). This is the check for an explicit `type:` field:
// present-and-canonical is trusted as-is; anything else (including a near-miss) is
// NOT silently corrected here — a task's frontmatter is data, not something to guess
// at, matching the fail-closed-on-ambiguity stance the rest of this package uses.
func IsCanonical(raw string, canonical map[string]bool) (string, bool) {
	n := normalize(raw)
	if n == "" || !canonical[n] {
		return "", false
	}
	return n, true
}

// Match normalizes raw against canonical, trying it AS-IS first, then with a
// trailing "-agent" appended (so "ci-infra" and "devops" resolve to
// "ci-infra-agent"/"devops-agent" — the exact split this package exists to
// collapse), then with a trailing "-agent" stripped (the reverse case, harmless
// belt-and-braces). Returns "" ,false if raw is empty or nothing matches — NEVER
// guesses at a close-but-not-matching name.
func Match(raw string, canonical map[string]bool) (string, bool) {
	n := normalize(raw)
	if n == "" {
		return "", false
	}
	if canonical[n] {
		return n, true
	}
	if canonical[n+"-agent"] {
		return n + "-agent", true
	}
	if trimmed := strings.TrimSuffix(n, "-agent"); trimmed != n && canonical[trimmed] {
		return trimmed, true
	}
	return "", false
}

// Resolve is the shared inference order both callers use: (1) an explicit type
// field, if canonical — kept as-is, idempotent; (2) the owner field, normalized
// against canonical; (3) the lane the task currently sits in (tried directly, then
// reverse-resolved through ROLES to its role name and tried again — a lane name is
// sometimes the type-ish name itself, e.g. "devops", and sometimes only resolvable
// via its ROLES role, e.g. "cr-1" -> role "code-review" -> "code-review-agent").
// ok=false means none of the three resolved — the caller's "manual", never guessed.
func Resolve(explicitType, owner, lane string, laneToRole map[string]string, canonical map[string]bool) (typ, source string, ok bool) {
	if t, ok := IsCanonical(explicitType, canonical); ok {
		return t, "type", true
	}
	if t, matched := Match(owner, canonical); matched {
		return t, "owner", true
	}
	if lane != "" {
		if t, matched := Match(lane, canonical); matched {
			return t, "lane", true
		}
		if role, found := laneToRole[lane]; found {
			if t, matched := Match(role, canonical); matched {
				return t, "lane", true
			}
		}
	}
	return "", "", false
}
