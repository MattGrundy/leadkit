// Package install is `leadkit init`'s business logic: computing what would change in
// settings.json to wire the leadkit guard hooks in, and — only for --apply — actually
// writing it. This is the FIRST cut (hooks only); the waker daemon and heartbeat cron
// are a later init part (see docs/build-tasks/init-hooks.md's scope note) — TODO:
// wire those here once that's built.
//
// SAFETY MODEL: ComputePlan never touches disk — it's a pure function over the
// existing settings.json bytes (or nil, meaning "start from {}"). Dry-run (the
// default) calls ComputePlan and stops. Only --apply calls Apply, which backs up the
// existing file (if any) and writes atomically (temp file + rename) — see Apply.
//
// IDEMPOTENT + SELF-HEALING, NEVER TOUCHES NON-LEADKIT ENTRIES: ComputePlan never
// touches any settings.json key other than hooks.PreToolUse, and within that never
// touches an entry that isn't recognized (via hookwire.Invokes — canonicalized bare
// vs. absolute-path form, see hookwire's commandPattern) as invoking one of
// hookwire.ExpectedHooks. For each expected hook it does exactly one of three things:
//   - not wired anywhere yet → APPEND one new matcher-group entry (an Addition), under
//     the hook's FULL intended matcher set (hookwire.HookSpec.InitMatcher — usage-logger
//     wires the alternation "Bash|Agent|Task" so a fresh install fires it on BOTH its
//     tool-telemetry and spawn-telemetry events).
//   - wired exactly once, already in the canonical `binary+" hook "+name` form → leave
//     it alone, even if the matcher is wrong (AlreadyWired — see below). Also left alone
//     (no churn) is a single entry whose command differs from canonical ONLY in the
//     binary token, when that token resolves via filepath.EvalSymlinks to the SAME real
//     file as our canonical binary — a symlink-deployed install (~/.claude/leadkit-bin
//     vs a ~/.local/bin/leadkit symlink to it). See sameRealBinary.
//   - wired in any OTHER shape (bare when binary is absolute, a genuinely stale absolute
//     path, or more than one entry at all) → REPAIR: remove every entry invoking that
//     hook from wherever it lives, then append exactly one fresh canonical entry. This
//     is what fixes the live multi-session duplication (multiple `init --apply` runs
//     each piling on a bare hook set) and the bare-command command-not-found failure
//     mode in one pass. Crucially, the rebuilt entry's matcher is the UNION of the
//     hook's intended set and every matcher the removed entries fired under
//     (repairMatcher) — so a consolidation NEVER reduces the set of tool events a hook
//     fires on (it must not collapse usage-logger's deliberate Bash + Agent|Task wiring
//     down to a single matcher, silently dropping spawn telemetry).
//
// The ONE exception to "leave a single canonical entry alone regardless of matcher":
// ComputePlan still never silently reassigns a matcher on its own — a lone entry
// that's already the exact canonical command, just under the wrong matcher, is left
// exactly as-is (a misconfiguration `leadkit doctor` flags; a human fixes it by hand).
// Repair only fires when the COMMAND form itself is wrong (bare/duplicate/stale), and
// when it does, the fresh entry gets the correct matcher as part of being rebuilt from
// scratch — so a repair also happens to fix an accompanying matcher problem, but
// ComputePlan never rewrites a matcher on an otherwise-correct single entry.
package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/hookwire"
)

// preToolUseEvent is the only settings.json hook event this cut of init touches —
// every current leadkit guard hook is PreToolUse (see hookwire.ExpectedHooks).
const preToolUseEvent = "PreToolUse"

// Plan is what `leadkit init` would do, computed by ComputePlan.
type Plan struct {
	Additions    []string // human-readable "+ <event>[<matcher>] <command>" lines, one per new hook
	AlreadyWired []string // hook names already wired, in canonical form, left untouched
	Repairs      []string // human-readable notes: stale/duplicate entries consolidated to one canonical entry
	Redundant    []string // "<bash script> is now duplicated by leadkit hook <name>" notes
	Changed      bool     // whether Apply would actually write anything
	NewSettings  []byte   // the resulting settings.json content — only meaningful when Changed
}

// ComputePlan reads existing (nil/empty means "no settings.json yet — start from
// {}"), adds any leadkit hook entries that aren't already wired, and REPAIRS any that
// are wired in a stale or duplicated form (bare when it should be absolute, or more
// than one entry for the same hook) — idempotent either way: an already-wired hook in
// the exact canonical form is left untouched (see the package doc for the one
// exception, matcher-only misconfiguration). Returns the plan. binary is the absolute
// path every hook entry's command is built from (`binary + " hook " + name`) — callers
// should pass an absolute path (os.Executable()/ResolveBinaryPath), not a bare command
// name, per docs/build-tasks/init-hook-idempotency.md (bare fails command-not-found
// under the /bin/sh PreToolUse hooks run under).
//
// Never writes anything; never mutates existing. A malformed existing settings.json
// (invalid JSON, or a "hooks"/"hooks.PreToolUse" that isn't the shape expected) is
// refused with an error — init must never guess its way past a settings.json it
// can't safely round-trip.
func ComputePlan(existing []byte, binary string) (Plan, error) {
	root := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &root); err != nil {
			return Plan{}, fmt.Errorf("install: existing settings.json is not valid JSON: %w", err)
		}
	}

	hooksRoot := map[string]json.RawMessage{}
	if raw, ok := root["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooksRoot); err != nil {
			return Plan{}, fmt.Errorf("install: existing settings.json's \"hooks\" is not an object: %w", err)
		}
	}

	var preToolUse []hookwire.MatcherGroup
	if raw, ok := hooksRoot[preToolUseEvent]; ok {
		if err := json.Unmarshal(raw, &preToolUse); err != nil {
			return Plan{}, fmt.Errorf("install: existing settings.json's \"hooks.%s\" is not an array: %w", preToolUseEvent, err)
		}
	}

	var plan Plan
	for _, spec := range hookwire.ExpectedHooks {
		matches := matchingEntries(preToolUse, spec.Name)
		matcher := spec.InitMatcher()
		canonical := binary + " hook " + spec.Name

		switch {
		case len(matches) == 0:
			// Not wired anywhere — add it, under this hook's full intended matcher set
			// (spec.InitMatcher: usage-logger wires the alternation "Bash|Agent|Task"
			// so a fresh install covers BOTH its tool-telemetry and spawn-telemetry
			// events, not just Bash).
			preToolUse = append(preToolUse, hookwire.MatcherGroup{
				Matcher: matcher,
				Hooks:   []hookwire.HookCommand{{Type: "command", Command: canonical}},
			})
			plan.Additions = append(plan.Additions, fmt.Sprintf("+ %s[%s] %s", spec.Event, matcher, canonical))
			plan.Changed = true

		case len(matches) == 1 && matches[0].Command == canonical:
			// Exactly one entry, already in the exact canonical form — leave it alone,
			// even under the wrong matcher (see the package doc: that's a
			// misconfiguration, `leadkit doctor`'s job to flag, a human's job to fix;
			// ComputePlan never reassigns a matcher on an otherwise-correct entry).
			plan.AlreadyWired = append(plan.AlreadyWired, spec.Name)

		case len(matches) == 1 && sameRealBinary(matches[0].Command, spec.Name, binary):
			// Exactly one entry whose command string differs from canonical ONLY in the
			// binary token, and that token resolves (via EvalSymlinks) to the SAME real
			// file as our canonical binary — a symlink-deployed install (e.g. the entry
			// runs ~/.claude/leadkit-bin while canonical is ~/.local/bin/leadkit, a
			// symlink to it). Rewriting it would be pure churn, so treat it as already
			// wired and leave it exactly as-is (same matcher-misconfig posture as the
			// exact-string case above). See sameRealBinary for the degrade-safe details.
			plan.AlreadyWired = append(plan.AlreadyWired, spec.Name)

		default:
			// Wired, but in some OTHER shape: a lone bare/stale-absolute entry, or more
			// than one entry (bare + absolute, plain duplicates, OR the SAME hook
			// deliberately spread across more than one matcher) — consolidate. Remove
			// every entry invoking this hook from wherever it lives (never touching
			// entries for any OTHER hook, leadkit or not, that share a matcher group
			// with it), then add back exactly one canonical entry whose matcher is the
			// UNION of this hook's intended set and every matcher the existing entries
			// fired under (repairMatcher) — so consolidation NEVER reduces the set of
			// tool events a hook fires on (the usage-logger Bash + Agent|Task regression).
			matcher = repairMatcher(matcher, matches)
			preToolUse = removeInvoking(preToolUse, spec.Name)
			preToolUse = append(preToolUse, hookwire.MatcherGroup{
				Matcher: matcher,
				Hooks:   []hookwire.HookCommand{{Type: "command", Command: canonical}},
			})
			plan.Repairs = append(plan.Repairs, fmt.Sprintf("%s: consolidated %d stale/duplicate entry(ies) -> %s[%s] %s", spec.Name, len(matches), spec.Event, matcher, canonical))
			plan.Changed = true
		}
	}

	// Redundancy notes: scan the settings as they'll stand AFTER this plan (existing
	// entries plus whatever we just added) for a known bash equivalent alongside its
	// now-present leadkit hook — reported for every hook that ends up wired, not just
	// newly-added ones (a bash script left over from before init was ever run is just
	// as worth flagging).
	after := hookwire.EvaluateWiring(map[string][]hookwire.MatcherGroup{preToolUseEvent: preToolUse})
	plan.Redundant = redundancyNotes(preToolUse, after)

	if !plan.Changed {
		plan.NewSettings = existing
		return plan, nil
	}

	hooksData, err := json.Marshal(preToolUse)
	if err != nil {
		return Plan{}, fmt.Errorf("install: marshal hooks.%s: %w", preToolUseEvent, err)
	}
	hooksRoot[preToolUseEvent] = hooksData

	hooksBlockData, err := json.Marshal(hooksRoot)
	if err != nil {
		return Plan{}, fmt.Errorf("install: marshal hooks: %w", err)
	}
	root["hooks"] = hooksBlockData

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return Plan{}, fmt.Errorf("install: marshal settings.json: %w", err)
	}
	out = append(out, '\n')
	plan.NewSettings = out
	return plan, nil
}

// wiredEntry is one settings.json hook entry that invokes a given leadkit hook, paired
// with the matcher of the group it lives under — ComputePlan needs BOTH the command
// (to spot the canonical/symlink-equivalent forms) and the matcher (to union coverage
// when consolidating, so a repair never drops an intentional second matcher).
type wiredEntry struct {
	Command string
	Matcher string
}

// matchingEntries returns every hook entry across groups that invokes the named leadkit
// hook (hookwire.Invokes — canonicalized bare-vs-absolute, so a bare and an
// absolute-path entry for the SAME hook both show up here), each with the matcher of
// the group it sits in. Reused by ComputePlan to decide add/leave-alone/repair and to
// preserve coverage across a consolidation.
func matchingEntries(groups []hookwire.MatcherGroup, name string) []wiredEntry {
	var out []wiredEntry
	for _, g := range groups {
		for _, h := range g.Hooks {
			if hookwire.Invokes(h.Command, name) {
				out = append(out, wiredEntry{Command: h.Command, Matcher: g.Matcher})
			}
		}
	}
	return out
}

// evalSymlinks is filepath.EvalSymlinks, indirected through a package var so tests can
// exercise the error path (Fix 2 must degrade safely — an EvalSymlinks failure behaves
// exactly as today: repair). Production always uses the real resolver.
var evalSymlinks = filepath.EvalSymlinks

// sameRealBinary reports whether command's leadkit binary token and canonicalBinary
// resolve, via filepath.EvalSymlinks (best-effort), to the SAME real file — i.e. the
// entry runs a symlink-equivalent of our canonical binary and rewriting it would be
// pure churn. Only consulted AFTER the exact-string canonical check has already failed
// (so this is the "strings differ, resolve the symlink" fallback, and only for the
// leadkit binary token — never arbitrary sibling entries). Any EvalSymlinks error, or
// an empty/absent binary token (a bare `leadkit hook ...`, which genuinely needs the
// bare->absolute repair), yields false so the caller repairs exactly as it did before.
func sameRealBinary(command, name, canonicalBinary string) bool {
	tok := hookwire.BinaryToken(command, name)
	if tok == "" {
		return false
	}
	realTok, err := evalSymlinks(tok)
	if err != nil {
		return false
	}
	realCanon, err := evalSymlinks(canonicalBinary)
	if err != nil {
		return false
	}
	return realTok == realCanon
}

// repairMatcher is the matcher a consolidation writes for its single rebuilt entry: the
// UNION of intended (this hook's InitMatcher) and every matcher the existing entries
// fired under, so a repair NEVER reduces the set of tool events the hook fires on.
// Intended tokens come first (canonical order); any extra tokens the existing entries
// covered are appended in order of first appearance. A bare "*" in ANY existing entry
// (fires on everything) is preserved as "*" rather than narrowed to an alternation.
func repairMatcher(intended string, entries []wiredEntry) string {
	for _, e := range entries {
		if strings.TrimSpace(e.Matcher) == "*" {
			return "*"
		}
	}
	seen := map[string]bool{}
	var tokens []string
	add := func(matcher string) {
		for _, tok := range strings.Split(matcher, "|") {
			tok = strings.TrimSpace(tok)
			if tok == "" || seen[tok] {
				continue
			}
			seen[tok] = true
			tokens = append(tokens, tok)
		}
	}
	add(intended)
	for _, e := range entries {
		add(e.Matcher)
	}
	return strings.Join(tokens, "|")
}

// removeInvoking returns groups with every hook entry invoking the named leadkit hook
// stripped out — wherever it lives, however many there are — leaving every OTHER entry
// (any other leadkit hook, and every non-leadkit bash guard) exactly as it was. A group
// left with zero hooks after stripping is dropped entirely rather than kept as a
// dangling `{"matcher": ..., "hooks": []}`.
func removeInvoking(groups []hookwire.MatcherGroup, name string) []hookwire.MatcherGroup {
	out := make([]hookwire.MatcherGroup, 0, len(groups))
	for _, g := range groups {
		kept := make([]hookwire.HookCommand, 0, len(g.Hooks))
		for _, h := range g.Hooks {
			if !hookwire.Invokes(h.Command, name) {
				kept = append(kept, h)
			}
		}
		if len(kept) > 0 {
			out = append(out, hookwire.MatcherGroup{Matcher: g.Matcher, Hooks: kept})
		}
	}
	return out
}

// redundancyNotes reports, for every leadkit hook that ends up wired in groups, any
// known bash-equivalent command string also present — a leftover the user may now
// want to retire (never removed automatically; see hookwire.KnownBashEquivalent).
func redundancyNotes(groups []hookwire.MatcherGroup, wired []hookwire.WireStatus) []string {
	wiredSet := map[string]bool{}
	for _, st := range wired {
		if st.Wired {
			wiredSet[st.Name] = true
		}
	}
	var notes []string
	seen := map[string]bool{} // dedupe: the same bash command can legitimately appear
	// under more than one matcher group (e.g. usage-logger.sh wired for both Bash and
	// Agent|Task) — one note per distinct command is enough, it's the same script.
	for name, bashSuffix := range hookwire.KnownBashEquivalent {
		if bashSuffix == "" || !wiredSet[name] {
			continue
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, bashSuffix) && !seen[name+"\x00"+h.Command] {
					seen[name+"\x00"+h.Command] = true
					notes = append(notes, fmt.Sprintf("%s duplicates `leadkit hook %s` — you may remove it once you trust the leadkit version (never auto-removed)", h.Command, name))
				}
			}
		}
	}
	return notes
}

// FormatPlan renders plan as `leadkit init`'s human-readable output: the diff (what
// would be added), what would be repaired, what's already wired, any redundancy notes
// — never the dry-run/applied trailer, which the caller (cmd/leadkit) prints itself
// since it depends on whether --apply was passed.
func FormatPlan(plan Plan) string {
	var b bytes.Buffer
	if len(plan.Additions) == 0 && len(plan.Repairs) == 0 {
		b.WriteString("leadkit init: nothing to add — every leadkit hook is already wired\n")
	} else if len(plan.Additions) > 0 {
		b.WriteString("leadkit init: the following would be added to settings.json:\n")
		for _, a := range plan.Additions {
			fmt.Fprintf(&b, "  %s\n", a)
		}
	}
	if len(plan.Repairs) > 0 {
		b.WriteString("leadkit init: the following would be REPAIRED (stale/bare/duplicate entries consolidated to one absolute-path entry):\n")
		for _, r := range plan.Repairs {
			fmt.Fprintf(&b, "  ~ %s\n", r)
		}
	}
	if len(plan.AlreadyWired) > 0 {
		fmt.Fprintf(&b, "already wired (untouched): %v\n", plan.AlreadyWired)
	}
	if len(plan.Redundant) > 0 {
		b.WriteString("redundant bash hooks (not removed):\n")
		for _, r := range plan.Redundant {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	return b.String()
}

// Apply performs init's actual write: nothing at all if plan.Changed is false (a
// pure no-op, no backup taken for nothing gained); otherwise a timestamped backup of
// path's current content (if it exists) followed by an ATOMIC write (temp file in the
// same directory, then rename) of plan.NewSettings. Returns the backup path ("" if
// none was needed).
func Apply(path string, plan Plan) (backupPath string, err error) {
	if !plan.Changed {
		return "", nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("install: mkdir %s: %w", dir, err)
	}

	if existing, err := os.ReadFile(path); err == nil {
		backupPath = fmt.Sprintf("%s.bak-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(backupPath, existing, 0o644); err != nil {
			return "", fmt.Errorf("install: write backup %s: %w", backupPath, err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("install: read %s for backup: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".leadkit-settings-*.tmp")
	if err != nil {
		return backupPath, fmt.Errorf("install: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(plan.NewSettings); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return backupPath, fmt.Errorf("install: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return backupPath, fmt.Errorf("install: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return backupPath, fmt.Errorf("install: rename into place: %w", err)
	}
	return backupPath, nil
}
