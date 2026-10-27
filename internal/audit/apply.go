package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/review"
)

// `leadkit audit --apply` is the ONLY mutating surface in this package — everything
// else here (DiskLens, SessionLens) is strictly read-only. Apply fixes exactly three
// SAFE, non-routing categories:
//
//  1. dangling      — reap a broken overlay symlink (target gone). Store untouched.
//  2. noncanonical  — rewrite a store task's frontmatter `status` ONLY when its value
//     is a KNOWN alias (opts.Aliases); an unrecognised/malformed value is reported in
//     Manual, never guessed at or rewritten.
//  3. done-drift    — the UNAMBIGUOUS subset of status-drift: a task whose frontmatter
//     status is already TERMINAL (completed/done/dropped/superseded) but whose
//     overlay symlink still sits in an ACTIVE status dir gets relocated to done/
//     under the same lane, via queue.Move (stays inside the queue-root guard). The
//     reverse (an active-status task sitting in done/) is ambiguous — could be a
//     deliberate reopen — and is never touched here.
//
// Never routes an orphan (no existing overlay placement is created), never deletes a
// store task file, never writes any frontmatter field other than `status`.
//
// Idempotent by construction: Apply always re-scans current disk state rather than
// working off a stale finding list, so a second run after a clean one finds nothing
// left to do.

// ApplyFix is one identified fix — present in ApplyResult.Fixes whether or not it was
// actually written (DryRun still reports the full plan).
type ApplyFix struct {
	Category string // "dangling" | "noncanonical" | "done-drift"
	Target   string // the task's id (or filename, if it has no numeric id)
	Action   string // "reap" | "normalize" | "relocate"
	From     string // queue-root-relative path, or the raw status value
	To       string // "" (reap), the canonical status value (normalize), or the queue-root-relative dest (relocate)
}

// ApplyOptions selects which categories Apply fixes/previews and the profile inputs
// each needs. DryRun=true (the default the CLI wires) computes and returns the exact
// same Fixes/Manual a real run would, but writes nothing anywhere.
type ApplyOptions struct {
	Dangling     bool
	Noncanonical bool
	DoneDrift    bool

	// Aliases is the KNOWN status-alias map (profile status_aliases) — only an exact
	// match (after normalizeStatus) is rewritten.
	Aliases map[string]string
	// CanonicalStatuses is the profile's canonical_statuses — a status already in
	// this set needs no fix, known-alias or otherwise.
	CanonicalStatuses []string

	DryRun bool
	// LogPath is where actually-applied fixes are appended as JSONL (see
	// WriteApplyLog). Ignored when DryRun; "" disables logging.
	LogPath string
}

// ApplyResult is one Apply run's outcome.
type ApplyResult struct {
	Fixes   []ApplyFix // every fix identified, in category order (dangling, noncanonical, done-drift)
	Manual  []Finding  // noncanonical values with no known alias — reported, never touched
	Applied bool       // whether Fixes were actually written (== !DryRun)
	Errors  []string   // per-fix failures; a failure skips just that fix, never aborts the run
}

// terminalStatuses are the frontmatter status values done-drift treats as "this task
// is finished" — the trigger side of the relocation. The reverse case (one of these
// literal dir names holding an ACTIVE-status task) is ambiguous and never handled here.
var terminalStatuses = map[string]bool{
	"completed": true, "done": true, "dropped": true, "superseded": true,
}

// Apply scans queueRoot/storeRoot fresh (never a cached finding list) and fixes (or,
// with DryRun, just plans) every category opts enables. See the package doc above for
// exactly what each of the three categories does and does not touch.
func Apply(queueRoot, storeRoot string, opts ApplyOptions) (ApplyResult, error) {
	var res ApplyResult
	res.Applied = !opts.DryRun

	if opts.Dangling {
		fixes, errs, err := applyDangling(queueRoot, opts.DryRun)
		if err != nil {
			return res, err
		}
		res.Fixes = append(res.Fixes, fixes...)
		res.Errors = append(res.Errors, errs...)
	}

	// noncanonical and done-drift both need the store's current frontmatter statuses —
	// read once, share between the two passes below.
	var storeEntries []os.DirEntry
	if opts.Noncanonical || opts.DoneDrift {
		var err error
		storeEntries, err = os.ReadDir(storeRoot)
		if err != nil {
			return res, fmt.Errorf("audit apply: read store root %s: %w", storeRoot, err)
		}
	}

	canonicalSet := map[string]bool{}
	for _, s := range opts.CanonicalStatuses {
		canonicalSet[normalizeStatus(s)] = true
	}
	aliases := map[string]string{}
	for from, to := range opts.Aliases {
		aliases[normalizeStatus(from)] = to
	}

	statusByBase := map[string]string{} // base filename -> raw frontmatter status, for done-drift below
	for _, e := range storeEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(storeRoot, e.Name())
		fm, err := review.ReadFrontmatter(path)
		if err != nil {
			continue // best-effort, matching DiskLens: no/malformed frontmatter is out of scope
		}
		raw := fm["status"]
		statusByBase[e.Name()] = raw

		if !opts.Noncanonical || raw == "" {
			continue
		}
		norm := normalizeStatus(raw)
		if canonicalSet[norm] {
			continue // already sanctioned — nothing to do
		}
		id := idOrBase(idFromFilename(e.Name()), e.Name())
		if canonical, isAlias := aliases[norm]; isAlias {
			fix := ApplyFix{Category: "noncanonical", Target: id, Action: "normalize", From: raw, To: canonical}
			if !opts.DryRun {
				if err := review.UpdateFrontmatter(path, []review.KV{{Key: "status", Value: canonical}}); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("noncanonical %s: %v", e.Name(), err))
					continue
				}
			}
			res.Fixes = append(res.Fixes, fix)
			continue
		}
		res.Manual = append(res.Manual, Finding{
			Category: CategoryNoncanonicalStatus, Severity: SeverityWarn,
			ID:      id,
			Detail:  fmt.Sprintf("%s: status %q has no known alias — needs manual attention", e.Name(), raw),
			RefSrc:  "store",
			RefPath: e.Name(),
		})
	}

	if opts.DoneDrift {
		fixes, manual, errs, err := applyDoneDrift(queueRoot, statusByBase, opts.DryRun)
		if err != nil {
			return res, err
		}
		res.Fixes = append(res.Fixes, fixes...)
		res.Manual = append(res.Manual, manual...)
		res.Errors = append(res.Errors, errs...)
	}

	if !opts.DryRun {
		if err := WriteApplyLog(opts.LogPath, res.Fixes); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("change log: %v", err))
		}
	}

	return res, nil
}

// applyDangling reaps every overlay symlink whose target is gone. walkOverlay already
// skips `_`-prefixed dirs at any depth (the waker/audit's own test-fixture rule), so
// that exclusion is inherited automatically here.
func applyDangling(queueRoot string, dryRun bool) ([]ApplyFix, []string, error) {
	overlay, err := walkOverlay(queueRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("audit apply: walk queue root %s: %w", queueRoot, err)
	}
	var fixes []ApplyFix
	var errs []string
	for _, oe := range overlay {
		if _, statErr := os.Stat(oe.Path); statErr == nil {
			continue // resolves fine
		}
		rel, relErr := filepath.Rel(queueRoot, oe.Path)
		if relErr != nil {
			rel = oe.Path
		}
		if !dryRun {
			if err := os.Remove(oe.Path); err != nil {
				errs = append(errs, fmt.Sprintf("dangling %s: %v", oe.Base, err))
				continue
			}
		}
		fixes = append(fixes, ApplyFix{Category: "dangling", Target: oe.Base, Action: "reap", From: rel, To: ""})
	}
	return fixes, errs, nil
}

// inProgressDirName is the one active status dir done-drift must NEVER move a task
// out of — it's an active CLAIM, not just "some active bucket". A terminal-status
// task found here is reported (Manual), never auto-relocated: post-incident fix,
// see docs/build-tasks/apply-safety-fixes.md Fix 1. Only the hyphenated spelling is
// checked (the live overlay's actual dir name) — normalizeStatus folds it to
// "in_progress" for the comparison below, same as every other status compare here.
const inProgressNorm = "in_progress"

// applyDoneDrift relocates every overlay entry whose (already-resolved, non-dangling)
// target's frontmatter status is terminal but which still sits in a NON-CLAIM active
// status dir (e.g. pending/) — via queue.Move, so the queue-root containment guard
// applies to this mutation exactly as it does to every other queue relink.
// in-progress/ is excluded entirely: a terminal status there is reported (manual),
// never auto-moved — see inProgressNorm.
func applyDoneDrift(queueRoot string, statusByBase map[string]string, dryRun bool) ([]ApplyFix, []Finding, []string, error) {
	overlay, err := walkOverlay(queueRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("audit apply: walk queue root %s: %w", queueRoot, err)
	}
	var fixes []ApplyFix
	var manual []Finding
	var errs []string
	for _, oe := range overlay {
		if _, statErr := os.Stat(oe.Path); statErr != nil {
			continue // dangling — that's the dangling category's job, not this one
		}
		raw, ok := statusByBase[oe.Base]
		if !ok || raw == "" || !terminalStatuses[normalizeStatus(raw)] {
			continue
		}
		normOverlay := normalizeStatus(oe.Status)
		if normOverlay == inProgressNorm {
			// An active claim holding an already-terminal-status task — report, never
			// touch. A live agent may be mid-work on it; moving the symlink out from
			// under that would be unsafe regardless of what the frontmatter now says.
			manual = append(manual, Finding{
				Category: CategoryStatusDrift, Severity: SeverityWarn,
				ID:      idOrBase(idFromFilename(oe.Base), oe.Base),
				Detail:  fmt.Sprintf("%s: terminal status %q sitting in in-progress/ — not auto-moved (active claim), needs manual review", oe.Base, raw),
				RefSrc:  "store",
				RefPath: oe.Base,
			})
			continue
		}
		if !activeStatuses[normOverlay] {
			continue // not in an active dir (e.g. already in done/, or some other bucket) — leave it
		}
		lane := filepath.Base(filepath.Dir(filepath.Dir(oe.Path)))
		dest := filepath.Join(queueRoot, lane, "done", oe.Base)
		relFrom, _ := filepath.Rel(queueRoot, oe.Path)
		relTo, _ := filepath.Rel(queueRoot, dest)
		if !dryRun {
			if err := queue.Move(queueRoot, oe.Path, dest, false); err != nil {
				errs = append(errs, fmt.Sprintf("done-drift %s: %v", oe.Base, err))
				continue
			}
		}
		fixes = append(fixes, ApplyFix{Category: "done-drift", Target: oe.Base, Action: "relocate", From: relFrom, To: relTo})
	}
	return fixes, manual, errs, nil
}

// applyLogEntry is one audit_apply_log JSONL record.
type applyLogEntry struct {
	TS       string `json:"ts"`
	Category string `json:"category"`
	Target   string `json:"target"`
	Action   string `json:"action"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// WriteApplyLog appends one JSONL record per fix to path. "" path or an empty fixes
// slice is a no-op. The mutations themselves have ALREADY happened by the time this
// is called (Apply only calls it post-mutation) — a logging failure is reported to
// the caller but never undoes or blocks the fixes it's trying to record.
func WriteApplyLog(path string, fixes []ApplyFix) error {
	if path == "" || len(fixes) == 0 {
		return nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("audit: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", path, err)
	}
	defer f.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, fx := range fixes {
		data, err := json.Marshal(applyLogEntry{
			TS: now, Category: fx.Category, Target: fx.Target, Action: fx.Action, From: fx.From, To: fx.To,
		})
		if err != nil {
			continue
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("audit: write %s: %w", path, err)
		}
	}
	return nil
}

// FormatApplyResult renders res as `leadkit audit --apply`'s human-readable output:
// every fix (or planned fix), any manual-attention items, and a per-category N
// planned / N applied summary.
func FormatApplyResult(res ApplyResult) string {
	var b strings.Builder
	verb := "would"
	if res.Applied {
		verb = "did"
	}
	for _, f := range res.Fixes {
		switch f.Action {
		case "reap":
			fmt.Fprintf(&b, "[%s] %s %s: reap dangling symlink %s\n", f.Category, verb, f.Target, f.From)
		case "normalize":
			fmt.Fprintf(&b, "[%s] %s %s: status %q -> %q\n", f.Category, verb, f.Target, f.From, f.To)
		case "relocate":
			fmt.Fprintf(&b, "[%s] %s %s: relocate %s -> %s\n", f.Category, verb, f.Target, f.From, f.To)
		default:
			fmt.Fprintf(&b, "[%s] %s %s: %s (%s -> %s)\n", f.Category, verb, f.Target, f.Action, f.From, f.To)
		}
	}
	for _, m := range res.Manual {
		fmt.Fprintf(&b, "[manual] %s\n", m.String())
	}

	counts := map[string]int{}
	for _, f := range res.Fixes {
		counts[f.Category]++
	}
	for _, cat := range []string{"dangling", "noncanonical", "done-drift"} {
		n := counts[cat]
		if res.Applied {
			fmt.Fprintf(&b, "%s: %d applied\n", cat, n)
		} else {
			fmt.Fprintf(&b, "%s: %d planned\n", cat, n)
		}
	}
	for _, e := range res.Errors {
		fmt.Fprintf(&b, "ERROR: %s\n", e)
	}
	return b.String()
}
