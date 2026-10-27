package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CANONICAL SOURCE: ~/.claude/hooks/task-status-guard.sh. Ported profile-driven —
// the shell hardcoded a single REPO=/…/<project> path and that repo's tasks/ path plus
// the literal status vocabulary; this reads the profile's store_root, its terminal-ish
// trigger statuses and status_aliases instead, so it travels to every adopter.
//
// WHY (user-directed 2026-08-13): a 455-ticket audit couldn't be done mechanically
// because 838/1217 tickets carried no PR/sha to verify a completion claim against — a
// stall and a success looked identical. Requiring evidence at the moment a ticket
// leaves in_progress makes every future audit a diff, not an interpretation.
//
// FAILS OPEN, exactly like the shell's `set +e`: any parse error / empty payload /
// out-of-scope file / non-triggering status → silent allow.

// TaskStatusScope is the store-scope + status vocabulary that governs ONE target task
// file — resolved from the project that OWNS that file, not the session. StoreRoot is
// the RESOLVED absolute store root; TriggerStatuses are the canonical terminal-ish
// statuses that assert work exists (completed / in_review / awaiting_pr_review,
// intersected with the profile's canonical set); StatusAliases maps a noncanonical
// spelling written in the file to its canonical form (e.g. done -> completed) so an
// alias value triggers too; CanonicalLegend is the profile's full canonical vocabulary,
// printed in the guidance.
type TaskStatusScope struct {
	StoreRoot       string
	TriggerStatuses []string
	StatusAliases   map[string]string
	CanonicalLegend []string
}

// TaskStatusConfig is task-status-guard's inputs (kept a plain struct so this package's
// config dependency stays confined to pr-guard, like every other hook). Resolve is the
// injected per-target resolver: given the edited task file, it returns the TaskStatusScope
// of the PROJECT that owns that file (its store_root + status vocabulary), or the built-in
// default scope for a file under no known project — NEVER the session-ambient profile's.
// So a task file is guarded by ITS OWN project's evidence rule even when edited from a
// foreign session; a file under no store falls out of scope and fails open, exactly as
// today. The guard stays PURE — it loads no profiles; cmd/leadkit injects Resolve (see
// resolveTargetTaskStatus). Resolve == nil disables the guard (fail open).
type TaskStatusConfig struct {
	Resolve func(targetFile string) TaskStatusScope
}

// statusLinePattern captures the value of the first `status:` line (frontmatter is at
// column 0). Mirrors the shell's `^status:[[:space:]]*<value>` with awk field 2.
var statusLinePattern = regexp.MustCompile(`(?m)^status:[ \t]*(\S+)`)

// refScalarOrListStartPattern matches a POPULATED `prs:`/`sha:`/`output:`/`no_pr_reason:`
// inline value — either a real first list element (`[ 748`) or a non-`[` scalar. Based
// on the shell's `^(prs|sha|output):[[:space:]]*(\[[[:space:]]*[^][:space:]]|[^[:space:]])`
// with two deliberate corrections to honour the spec's "empty claim is rejected" intent:
// the scalar branch excludes `[` (so `prs: []` / `prs: [ ]` fall through to the list
// branch, which requires a real element, instead of the `[` itself satisfying the
// fallback), and no_pr_reason is included so an explicit "no PR" reason in EITHER source
// passes.
var refScalarOrListStartPattern = regexp.MustCompile(`(?m)^(prs|sha|output|no_pr_reason):[ \t]*(\[[ \t]*[^\]\s]|[^\s\[])`)

// refKeyLinePattern / listItemPattern implement the shell's `grep -A2` block-list form:
// a `prs:`/`sha:`/`output:` key line whose following (up to two) lines carry a real
// `- item` entry (a bare `prs:` with an empty list under it must still be rejected).
var refKeyLinePattern = regexp.MustCompile(`^(prs|sha|output):`)

// listItemPattern matches a real YAML block-sequence entry (`- 748`) — REQUIRING
// whitespace after the dash, unlike the shell's `-[[:space:]]*\S`. That refinement is
// deliberate: the shell form also matched the frontmatter document delimiter `---`,
// which silently defeated the empty-claim rejection whenever a bare `prs:` sat next to
// a `---` line. A genuine list item always carries the `- ` space, so requiring it
// rejects `---` while still matching every real entry.
var listItemPattern = regexp.MustCompile(`^[ \t]*-[ \t]+\S`)

// TaskStatusGuard blocks an Edit/Write/MultiEdit that sets a task's status to a
// terminal-ish value without a populated prs/sha/output reference (or an explicit
// no_pr_reason), checking BOTH the written payload and the file's current contents.
func TaskStatusGuard(in Input, cfg TaskStatusConfig) Result {
	if in.ToolName != "Edit" && in.ToolName != "Write" && in.ToolName != "MultiEdit" {
		return allow
	}
	filePath := in.ToolInput.FilePath
	if filePath == "" {
		return allow
	}
	if cfg.Resolve == nil {
		return allow // no per-target resolver wired — fail open
	}
	scope := cfg.Resolve(filePath)
	if !inStoreRoot(filePath, scope.StoreRoot) {
		return allow
	}

	payload := writtenPayload(in.ToolInput)
	if payload == "" {
		return allow
	}

	m := statusLinePattern.FindStringSubmatch(payload)
	if m == nil {
		return allow
	}
	newStatus := m[1]
	if !isTriggerStatus(newStatus, scope) {
		return allow
	}

	// Two-source evidence: the reference may already live in the file untouched by
	// this edit. current-file read is best-effort (a not-yet-written file, an
	// unreadable path) — its absence just means "payload is the only source".
	fileContents, _ := os.ReadFile(filePath)
	if hasPopulatedRef(payload) || hasPopulatedRef(string(fileContents)) {
		return allow
	}

	return Result{Stderr: taskStatusGuidance(newStatus, filepath.Base(filePath), scope.CanonicalLegend), ExitCode: 2}
}

// writtenPayload extracts the text being written: new_string (Edit), content (Write),
// or every edit's new_string joined (MultiEdit). The shell read only new_string //
// content; MultiEdit is handled here too so a status flip buried in a multi-edit isn't
// silently missed.
func writtenPayload(ti toolInput) string {
	if ti.NewString != "" {
		return ti.NewString
	}
	if ti.Content != "" {
		return ti.Content
	}
	if len(ti.Edits) > 0 {
		parts := make([]string, 0, len(ti.Edits))
		for _, e := range ti.Edits {
			if e.NewString != "" {
				parts = append(parts, e.NewString)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// inStoreRoot reports whether filePath is a `.md` file under storeRoot (any depth),
// mirroring the shell's `"$REPO"/tasks/*.md` case (where `*` spans `/`). An empty
// storeRoot never matches (nothing is in scope → fail open).
func inStoreRoot(filePath, storeRoot string) bool {
	if storeRoot == "" {
		return false
	}
	if !strings.HasSuffix(filePath, ".md") {
		return false
	}
	root := filepath.Clean(storeRoot)
	clean := filepath.Clean(filePath)
	if root == string(filepath.Separator) {
		return true // the filesystem root: every task file is in scope (a store that could not be read)
	}
	return clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))
}

// isTriggerStatus reports whether newStatus (after resolving a known alias to its
// canonical spelling) is one of the terminal-ish statuses that assert work exists.
func isTriggerStatus(newStatus string, scope TaskStatusScope) bool {
	resolved := newStatus
	if canonical, ok := scope.StatusAliases[newStatus]; ok {
		resolved = canonical
	}
	for _, s := range scope.TriggerStatuses {
		if s == resolved {
			return true
		}
	}
	return false
}

// hasPopulatedRef reports whether text carries a POPULATED prs/sha/output reference (or
// an explicit no_pr_reason) — an inline scalar, a `[ item` list start, or a `- item`
// under a prs/sha/output key. A bare `prs:` or an empty `prs: []` is exactly the empty
// claim this guard rejects and returns false.
func hasPopulatedRef(text string) bool {
	if text == "" {
		return false
	}
	if refScalarOrListStartPattern.MatchString(text) {
		return true
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !refKeyLinePattern.MatchString(line) {
			continue
		}
		// The shell's `-A2`: the key line plus up to two following lines.
		for j := i; j < len(lines) && j <= i+2; j++ {
			if listItemPattern.MatchString(lines[j]) {
				return true
			}
		}
	}
	return false
}

func taskStatusGuidance(newStatus, base string, legend []string) string {
	legendLine := strings.Join(legend, ", ")
	return fmt.Sprintf(`Task status guard: refusing to set '%s' on %s with no reference.

'%s' asserts the work exists somewhere, but neither the edit nor the file carries a
populated 'prs:', 'sha:' or 'output:' entry. Without one there is nothing a later audit
can verify the claim against — which is how a stall becomes indistinguishable from a
success.

Do one of:
  - add the PR:                     prs: [748]      (or a '- 748' list item)
  - spike-branch work with no PR:   sha: ff260b367d9e...   (full sha — short prefixes
                                     collide across clones)
  - research/design/plan/reference: output: design/x.md   (where the result lives — that
                                     file IS the deliverable, so it is the evidence)
  - genuinely nothing to point at:  no_pr_reason: <why>
  - still in flight, leave it at:   status: in_progress
  - will never be done:             status: dropped

Canonical statuses:
  %s
`, newStatus, base, newStatus, legendLine)
}
