package queue

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Governed task delete (docs/build-tasks/queue-rm-governed-task-delete.md). The
// queue-guard blocks a raw rm/mv/ln on the queue by design, but there was NO governed
// way to DELETE a dead task: `queue move` only relocates, `repair` reaps only DANGLING
// links, `prune-empty-lanes` removes only EMPTY dirs. A real dead task FILE sitting in
// the flat store (with valid overlay links) therefore could not be removed the governed
// way at all. RemoveTask closes that gap: it removes a task COMPLETELY — the flat-store
// file AND every overlay symlink that resolves to it — dry-run by default, ordered so no
// live overlay link ever outlives the store file it points at, and only ever the named
// store file plus the symlinks that resolve to IT (never a dir, never another task,
// never a real non-symlink file). It is the governed alternative the queue-guard points
// operators at instead of a raw rm.

// terminalStatuses mirrors every other internal package's own copy (see e.g.
// internal/hierarchy/model.go) — duplicated on purpose, this codebase's small-package
// decoupling convention (queue must not import review/hierarchy: they import queue).
// A task in one of these is a completed/abandoned artifact safe to delete without the
// --allow-non-terminal flag; everything else (needs_rework/pending/in_progress/gated/…)
// is LIVE work and guarded.
var terminalStatuses = map[string]bool{
	"completed": true, "done": true, "dropped": true, "superseded": true,
}

// normalizeStatus lowercases, trims, and folds '-' to '_' so a status is matched the
// same way regardless of separator/casing (mirrors the other packages' copies).
func normalizeStatus(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// OverlayLink is one overlay symlink under the queue tree that resolves to the target
// store file — a link RemoveTask will (or, in dry-run, would) remove. Path is the
// absolute path to the symlink itself; RelPath is its queue-relative slash path (the
// full nested location, so a link deeper than the canonical <lane>/<status>/<entry> is
// still named exactly); Target is the readlink target as written.
type OverlayLink struct {
	Path    string
	RelPath string
	Target  string
}

// RemoveResult is a `queue rm` run's outcome: the resolved store file, its status and
// whether that status is terminal, every overlay link that resolves to it, whether the
// terminal guard would block (dry-run) or did block (apply), and — under apply — exactly
// what was removed plus any per-item failure.
type RemoveResult struct {
	Name      string        // the --task argument, verbatim
	StoreFile string        // the resolved <storeRoot>/<name> real file
	Status    string        // its frontmatter status (raw; "" when absent/unreadable)
	Terminal  bool          // Status is in the terminal set
	Links     []OverlayLink // EVERY overlay symlink that resolves to StoreFile

	Guarded bool // a non-terminal task with apply requested but --allow-non-terminal NOT passed → blocked

	Applied      bool          // an --apply run that was NOT guard-blocked
	RemovedLinks []OverlayLink // overlay symlinks actually removed (apply only)
	RemovedStore bool          // the store file actually removed (apply only)
	LinkErrors   []string      // per-link removal failures (best-effort; never abort the rest)
	StoreErr     string        // the store-file removal failure, if any

	DryRun bool
}

// resolveStoreFile resolves the --task name to exactly ONE real flat-store file under
// storeRoot. It considers the name as given and, only when the name lacks a .md suffix,
// name+".md". A candidate counts as a match ONLY when it exists as a REGULAR FILE — a
// directory or a symlink sharing the name is never a match (so we never resolve to, and
// therefore never delete, anything but a real store file). Zero matches → "no such task";
// more than one → ambiguous. Both refuse and remove nothing.
func resolveStoreFile(storeRoot, name string) (string, error) {
	if strings.TrimSpace(storeRoot) == "" {
		return "", fmt.Errorf("store root unknown — cannot resolve task %q (pass --store)", name)
	}
	cands := []string{name}
	if !strings.HasSuffix(name, ".md") {
		cands = append(cands, name+".md")
	}
	var found []string
	seen := map[string]bool{}
	for _, c := range cands {
		p := filepath.Join(storeRoot, c)
		if seen[p] {
			continue
		}
		seen[p] = true
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		// Regular file ONLY — IsRegular() is false for a symlink and for a directory, so a
		// same-named dir or symlink in the store is silently not-a-match (never resolved,
		// never removed), exactly the conservative behaviour we want.
		if fi.Mode().IsRegular() {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no such task in store: %q (looked for %s under %s) — nothing removed",
			name, strings.Join(cands, ", "), storeRoot)
	case 1:
		// Defensive (belt-and-suspenders, not the primary fix): return an absolute, cleaned
		// path so a downstream caller sees a consistent path shape. Correctness comes from
		// os.SameFile in findLinksToStore, NOT from this path massaging — if Abs fails we still
		// return the matched path unchanged rather than lose the resolution.
		if abs, aerr := filepath.Abs(found[0]); aerr == nil {
			return abs, nil
		}
		return found[0], nil
	default:
		return "", fmt.Errorf("ambiguous task %q: multiple store files match (%s) — pass the exact filename; nothing removed",
			name, strings.Join(found, ", "))
	}
}

// readStoreStatus reads the frontmatter `status:` of a store file. It mirrors
// review.ReadFrontmatter's flat key:value parsing (queue cannot import review — review
// imports queue), INCLUDING its not stripping quotes: `queue rm` decides whether a ticket is
// terminal from this, and a value it does not recognise (`"done"`) must stay non-terminal and
// guarded, the conservative default. A file with no readable frontmatter yields "".
func readStoreStatus(path string) string { return readFrontmatterField(path, "status") }

// readFrontmatterField returns the trimmed value of one flat `key: value` line in path's
// frontmatter block, exactly as written, or "" when there is none.
func readFrontmatterField(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return "" // block closed without the key
		}
		k, val, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// unquote removes one pair of matching quotes. Only the blocked check uses it, so that a quoted
// `status: "blocked"` is still recognised; it is deliberately NOT applied to readStoreStatus.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// ticketBlocked reports whether the ticket behind a queue entry is `status: blocked`, and why
// (its blocked_on). The entry is followed, so a symlink into tasks/ reads the real ticket.
func ticketBlocked(entry string) (blocked bool, blockedOn string) {
	if normalizeStatus(unquote(readStoreStatus(entry))) != "blocked" {
		return false, ""
	}
	return true, unquote(readFrontmatterField(entry, "blocked_on"))
}

// ticketTerminal reports whether the ticket behind a queue entry is in a terminal status (done,
// completed, dropped, superseded), and which. It is the one place the terminal set is read for a
// queue entry: wait skips such a ticket and sweep reports it, from the same answer.
func ticketTerminal(entry string) (terminal bool, status string) {
	status = normalizeStatus(unquote(readStoreStatus(entry)))
	return terminalStatuses[status], status
}

// findLinksToStore walks the WHOLE queue tree under root (any depth) exactly the way
// FindDangling does — filepath.WalkDir (Lstat-based, so it NEVER follows a directory
// symlink and cannot escape the tree), skipping the ROLES routing file and every plain
// (non-symlink) entry — and collects every SYMLINK whose target is the SAME underlying
// file as storeFile. Identity is decided by os.SameFile (device+inode), NOT by a string
// comparison of resolved paths: os.Stat follows the symlink to its target and SameFile
// compares the stat results, so a link matches regardless of path REPRESENTATION
// (`/var` vs `/private/var`, a relative walk root vs an absolute defaulted store, `..`
// segments) — the class of bug that made a relative --queue + defaulted --store silently
// find ZERO links. A plain file name-colliding with the task is never collected (only
// symlinks are). A dangling or otherwise unresolvable symlink fails os.Stat and is
// skipped (it cannot point at a file that exists, so it cannot be a live link to the
// store file).
func findLinksToStore(root, storeFile string) ([]OverlayLink, error) {
	if _, err := os.ReadDir(root); err != nil {
		return nil, fmt.Errorf("no queue dir at %s: %w", root, err)
	}
	// Stat the store file ONCE up front — os.Stat follows any symlink to the real file, so
	// fiStore identifies the underlying inode the overlay links must point at.
	fiStore, err := os.Stat(storeFile)
	if err != nil {
		return nil, fmt.Errorf("resolve store file %s: %w", storeFile, err)
	}
	var out []OverlayLink
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		if d.Name() == "ROLES" && !d.IsDir() {
			return nil
		}
		// Not a symlink? A real dir is descended (default); a plain file is NEVER a removal
		// target. WalkDir uses Lstat, so a dir symlink reports IsDir()==false and is handled
		// below as a symlink (never descended) — the walk can't escape the tree.
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		// A symlink: match iff it points at the SAME underlying file as the store file.
		// os.Stat follows the whole chain (including the leaf) to the target's inode; a
		// dangling/unresolvable link fails Stat → skipped (it cannot be a live link to an
		// existing store file). os.SameFile compares device+inode, so this is immune to path
		// representation — the correctness the string EvalSymlinks compare lacked.
		fiP, serr := os.Stat(p)
		if serr != nil || !os.SameFile(fiP, fiStore) {
			return nil
		}
		target, _ := os.Readlink(p)
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p
		}
		out = append(out, OverlayLink{Path: p, RelPath: filepath.ToSlash(rel), Target: target})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// RemoveTask is the governed task delete. It resolves the ONE real store file for name
// under storeRoot (ambiguous/missing → error, nothing removed), reads its status, and
// finds every overlay symlink under queueRoot that resolves to it. Then:
//
//   - TERMINAL GUARD: if the status is NON-terminal and apply is requested WITHOUT
//     allowNonTerminal, it removes NOTHING and returns Guarded=true (the caller refuses).
//     A dry-run always reports the status and whether the guard would block.
//   - DELETE ORDER (apply, not guarded): the overlay SYMLINKS are removed FIRST (each a
//     plain os.Remove of the symlink — never followed), THEN the store file. So there is
//     never an intermediate point where a live overlay link points at a just-deleted store
//     file. A per-link failure is recorded and does NOT abort the rest; the store-file
//     removal is attempted regardless and its own failure is recorded.
//
// In dry-run (apply=false) it mutates NOTHING and only reports the full plan.
func RemoveTask(queueRoot, storeRoot, name string, allowNonTerminal, apply bool) (RemoveResult, error) {
	storeFile, err := resolveStoreFile(storeRoot, name)
	if err != nil {
		return RemoveResult{Name: name, DryRun: !apply}, err
	}
	links, err := findLinksToStore(queueRoot, storeFile)
	if err != nil {
		return RemoveResult{Name: name, StoreFile: storeFile, DryRun: !apply}, err
	}
	status := readStoreStatus(storeFile)
	res := RemoveResult{
		Name:      name,
		StoreFile: storeFile,
		Status:    status,
		Terminal:  terminalStatuses[normalizeStatus(status)],
		Links:     links,
		DryRun:    !apply,
	}

	// Terminal guard: a live (non-terminal) task needs the explicit flag to be deleted.
	if !res.Terminal && apply && !allowNonTerminal {
		res.Guarded = true
		return res, nil // blocked — nothing removed
	}
	if !apply {
		return res, nil // dry-run — full plan reported, nothing changed
	}

	// APPLY. Overlay symlinks FIRST (os.Remove of a symlink removes the link, never the
	// target), so no live link ever outlives the store file it points at.
	res.Applied = true
	for _, l := range links {
		if rerr := os.Remove(l.Path); rerr != nil {
			res.LinkErrors = append(res.LinkErrors, fmt.Sprintf("%s: %v", l.RelPath, rerr))
			continue
		}
		res.RemovedLinks = append(res.RemovedLinks, l)
	}
	// THEN the store file.
	if rerr := os.Remove(storeFile); rerr != nil {
		res.StoreErr = rerr.Error()
	} else {
		res.RemovedStore = true
	}
	return res, nil
}

// FormatRemove renders a RemoveResult as the human report `leadkit queue rm` prints —
// the resolved store file, its status + the terminal-guard verdict, one line per overlay
// link, and a summary of what was (or would be) removed.
func FormatRemove(res RemoveResult) string {
	var b strings.Builder

	status := res.Status
	if status == "" {
		status = "(none)"
	}
	guardWord := "TERMINAL (safe to delete)"
	if !res.Terminal {
		guardWord = "NON-TERMINAL (LIVE) — needs --allow-non-terminal to delete"
	}
	fmt.Fprintf(&b, "queue-rm: task %s\n", res.Name)
	fmt.Fprintf(&b, "  store file: %s\n", res.StoreFile)
	fmt.Fprintf(&b, "  status: %s — %s\n", status, guardWord)

	if len(res.Links) == 0 {
		b.WriteString("  overlay links: none resolve to this store file\n")
	}
	for _, l := range res.Links {
		verb := "REMOVE"
		if !res.DryRun {
			verb = "removed"
			if res.Guarded {
				verb = "REMOVE"
			}
		}
		fmt.Fprintf(&b, "  %s link %s (→ %s)\n", verb, l.RelPath, l.Target)
	}

	switch {
	case res.Guarded:
		fmt.Fprintf(&b, "REFUSED — %s is non-terminal (status %s); nothing removed. Re-run with --allow-non-terminal to delete a live task.\n",
			res.Name, status)
	case res.DryRun:
		fmt.Fprintf(&b, "queue-rm: plan — would remove %d overlay link(s) THEN the store file (dry-run — nothing changed; re-run with --apply)\n", len(res.Links))
	default:
		fmt.Fprintf(&b, "queue-rm: removed %d overlay link(s)", len(res.RemovedLinks))
		if res.RemovedStore {
			b.WriteString(" and the store file")
		} else {
			b.WriteString("; store file NOT removed")
		}
		if res.StoreErr != "" {
			fmt.Fprintf(&b, " (store-remove error: %s)", res.StoreErr)
		}
		for _, e := range res.LinkErrors {
			fmt.Fprintf(&b, "; link-error: %s", e)
		}
		b.WriteString("\n")
	}
	return b.String()
}
