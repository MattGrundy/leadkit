package queue

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/roles"
)

// Self-repair (docs/build-tasks/self-surfacing-diagnostics.md, Part 4) reaps ONLY
// DANGLING queue symlinks — a link whose store target no longer exists. It NEVER
// touches a live ticket (a symlink that still resolves) and NEVER touches a plain
// file: the reap set is exactly the BROKEN set `queue sweep` reports, nothing more.
// Confined to the queue root the same way Move is, so a repair can't reach outside it.
//
// Two entry points share this: `leadkit queue repair` (explicit operator action,
// dry-run by default, --apply to reap — always available regardless of config) and
// the sweep's `diagnostics.self_repair: auto` mode (the daemon reaps each run). The
// `report`/`command` modes never call Apply — they only surface.

// DanglingLink is one reapable entry: a symlink under the queue whose target is gone.
// The reaper walks the whole queue tree RECURSIVELY, so a dangling link can sit at any
// depth (e.g. a stray double-nested queue/queue/<lane>/<status>/link). Lane/Status are a
// best-effort label derived from the path (first segment / immediate parent bucket); the
// full nested location is carried in relPath and rendered by Rel() so dry-run output names
// the exact path even when it is deeper than the canonical <lane>/<status>/<entry>.
type DanglingLink struct {
	Lane   string // first path segment under the queue root (best-effort label)
	Status string // the symlink's immediate parent dir name (the bucket, where meaningful)
	Name   string // the symlink's basename
	Path   string // absolute path to the symlink itself
	Target string // the (missing) target it points at

	relPath string // queue-relative path to the symlink itself (full nested location)
}

// Rel is the queue-relative path label used in logs and dry-run output — the FULL nested
// location of the symlink (e.g. queue/_gvtest-owner/pending/gate-_gvtest.md), so a link
// found deeper than the canonical three levels is still named exactly. Falls back to the
// lane/status/name join for a DanglingLink constructed without a relPath.
func (d DanglingLink) Rel() string {
	if d.relPath != "" {
		return d.relPath
	}
	return filepath.ToSlash(filepath.Join(d.Lane, d.Status, d.Name))
}

// bucketDirs are the three status buckets that define a lane's shape. A dir under the
// queue root is "lane-shaped" only if it holds all three as real (non-symlink) dirs.
var bucketDirs = []string{"pending", "in-progress", "done"}

// EmptyLane is a lane whose pending/, in-progress/, AND done/ are ALL empty — a prune
// candidate found by the opt-in --prune-empty-lanes pass. Registered is true when the
// lane name appears in <root>/ROLES: a registered role lane legitimately sits empty
// between work and is NEVER auto-pruned (reported skipped-registered instead).
type EmptyLane struct {
	Lane       string // lane dir name (first path segment under the queue root)
	Path       string // absolute path to the lane dir
	Registered bool   // present in <root>/ROLES — never auto-pruned even when empty
}

// StrayDir is a dir under the queue root that is NOT lane-shaped at all — a structural
// artifact (e.g. a double-nested <root>/queue/, or a dir missing the three buckets).
// Empty is true only when it is RECURSIVELY empty (holds no file anywhere): a stray
// with any file in it is reported but NEVER removed — a human must inspect it.
type StrayDir struct {
	Lane  string // the stray dir name
	Path  string // absolute path to the stray dir
	Empty bool   // recursively empty (no files at any depth) — removable, apply-only
}

// BucketAlias is a redundant separator-variant bucket symlink found under the opt-in
// --prune-empty-lanes pass: a symlink at the <lane>/<name> bucket level whose NAME is a
// separator-variant of a canonical bucket (e.g. `in_progress` for `in-progress`) AND
// whose target resolves to the canonical sibling bucket in the SAME lane. That is
// unambiguous cruft (leadkit uses the hyphen bucket exclusively) and Reap is true, so it
// is removed under -apply. A candidate whose target points ELSEWHERE (a different lane, a
// real alias inbox), or that sits in a REGISTERED lane, is reported with Reap=false and
// LEFT ALONE — never auto-removed (when in doubt, report, don't remove).
type BucketAlias struct {
	Lane   string // the lane dir the alias sits in
	Name   string // the alias symlink's basename (e.g. in_progress)
	Canon  string // the canonical bucket it aliases (e.g. in-progress)
	Path   string // absolute path to the alias symlink itself
	Target string // the readlink target (as written)
	Reap   bool   // true only for an unambiguous same-lane alias in an UNREGISTERED lane
}

// brokenKind classifies a dangling (os.Stat-fails) symlink for the repair-first reaper.
type brokenKind int

const (
	brokenFilePlacement brokenKind = iota // an entry-level task-FILE placement link
	brokenDirAlias                        // a directory / inbox ALIAS symlink — never auto-reaped
)

// RepointRepair is one REPAIRABLE placement: a broken FILE symlink whose task is ALIVE in
// the flat store (its target basename resolves to a real file under the store root) — the
// link is merely MISPOINTED (wrong relative depth), not orphaned. The repair-first reaper
// RE-POINTS it (atomic temp+rename) to the correct relative path and NEVER reaps it.
// Applied is true when the re-point was actually written to disk (apply only); Failed is
// true when an -apply re-point could not complete — in which case the ORIGINAL link is
// left EXACTLY as it was (never a missing placement) and Err says why.
type RepointRepair struct {
	Link      DanglingLink // the broken link being repaired
	OldTarget string       // the (wrong) target it pointed at
	NewTarget string       // the correct RELATIVE target from the link's dir to <store>/<basename>
	StorePath string       // the absolute live store file that proves the task alive
	Applied   bool         // the re-point was written (apply only)
	Failed    bool         // an -apply re-point failed; the ORIGINAL link is untouched
	Err       string       // why the re-point failed (Failed only)
}

// Rel proxies the underlying link's full nested path for logs/dry-run output.
func (r RepointRepair) Rel() string { return r.Link.Rel() }

// RepairResult is a repair run's outcome: what was (or, in dry-run, would be) reaped,
// repaired, or reported, plus (when the opt-in --prune-empty-lanes pass ran) the
// empty-lane / stray-dir prune.
type RepairResult struct {
	Dangling []DanglingLink // EVERY broken link found (repairs + orphans + dir-aliases + unverifiable)
	Reaped   []DanglingLink // populated only when applied; the ones actually removed

	// Repair-first classification of the broken set (docs/build-tasks/reaper-repair-first.md):
	StoreRoot     string          // the flat store root used to tell repairable from orphaned
	StoreReadable bool            // false → store root unknown/unreadable → nothing reaped, all UNVERIFIABLE
	Repairs       []RepointRepair // mispointed-but-LIVE links: re-pointed (apply) / would re-point (dry-run) — NEVER reaped
	Orphans       []DanglingLink  // truly-dead links (no live task in store): reaped (apply) / would reap (dry-run)
	DirAliases    []DanglingLink  // broken DIRECTORY/inbox aliases: reported, NEVER auto-reaped
	Unverifiable  []DanglingLink  // broken FILE links we cannot verify (store root unknown): reaped NOTHING

	// Prune is true when the opt-in empty-lane/stray-dir pass ran (regardless of apply).
	Prune         bool
	EmptyLanes    []EmptyLane   // all-buckets-empty lanes found (candidates AND registered-skipped)
	PrunedLanes   []EmptyLane   // empty unregistered lanes actually removed (apply only)
	StrayDirs     []StrayDir    // stray non-lane dirs found (empty AND file-holding)
	PrunedStray   []StrayDir    // recursively-empty stray dirs actually removed (apply only)
	BucketAliases []BucketAlias // redundant bucket-alias symlinks found (reapable AND left-alone)
	ReapedAliases []BucketAlias // unambiguous same-lane bucket aliases actually removed (apply only)
	PruneErrors   []string      // best-effort: a removal that failed is logged here, never aborts

	// Strays are the entries in a lane's pending/ or in-progress/ that are not tickets (strays.go).
	// Only the ones whose removal loses nothing (an empty directory tree, an empty file) are cleared,
	// and only under apply; the rest are reported for a human.
	Strays        []StrayEntry
	ClearedStrays []StrayEntry
	StrayErrors   []string

	DryRun bool
}

// FindDangling walks the WHOLE queue tree under root RECURSIVELY (any depth) and
// collects DANGLING symlinks only — an entry that is a symlink (Lstat) whose target does
// not resolve (Stat fails). A live ticket (Stat succeeds) and a plain file (not a
// symlink) are both skipped, exactly as before; the only change is depth: a dangling link
// nested past the canonical <lane>/<status>/<entry> three levels (e.g. under a stray
// double-nested queue/queue/) is now reached too, so `queue repair` no longer reports
// "0 dangling" while a nested broken link persists.
//
// It must NOT descend a SYMLINKED directory (an alias inbox) — filepath.WalkDir already
// uses Lstat and never follows a symlink (a dir symlink reports IsDir()==false, so its
// contents are never walked), which both preserves Sweep's alias-inbox skip AND is what
// keeps the walk from ever escaping the tree through a directory symlink. The ROLES
// routing file is skipped. The reaper does NOT name-skip a `tasks` entry inside the tree
// (that is the prune pass's concern): the real flat tasks/ store is a SIBLING of the
// queue root, never under it, so a walk under the queue never reaches it; a nested stray
// queue/tasks/ IS under the queue and is legitimately walked — but holds only real files,
// so nothing there is ever reaped.
func FindDangling(root string) ([]DanglingLink, error) {
	if _, err := os.ReadDir(root); err != nil {
		return nil, fmt.Errorf("no queue dir at %s: %w", root, err)
	}
	var out []DanglingLink
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry — skip it (and, if a dir, its subtree), never abort the walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		// Skip the ROLES routing file — never a queue entry (and it is a plain file, so it
		// would be skipped below regardless; skipped by name for clarity/robustness).
		if d.Name() == "ROLES" && !d.IsDir() {
			return nil
		}
		// Not a symlink? A real directory is descended (default); a plain file is never a
		// repair target. WalkDir does not follow dir symlinks (Lstat: IsDir()==false for a
		// symlink), so the alias-inbox skip holds and the walk cannot escape the tree.
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		// A symlink: dangling iff its target does not resolve (Stat fails). A live ticket
		// (or a live alias-inbox dir symlink) resolves and is skipped — and, being a
		// symlink, is never descended into.
		if _, serr := os.Stat(p); serr == nil {
			return nil // target resolves — LIVE, never reaped
		}
		target, _ := os.Readlink(p)
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			rel = p
		}
		out = append(out, danglingFromRel(filepath.ToSlash(rel), p, target))
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel() < out[j].Rel() })
	return out, nil
}

// danglingFromRel builds a DanglingLink from a queue-relative slash path to the symlink.
// Lane is the first segment, Status the symlink's immediate parent dir (the bucket, where
// the path is deep enough), Name the basename; relPath carries the full nested location so
// Rel() names it exactly even below the canonical three levels.
func danglingFromRel(rel, path, target string) DanglingLink {
	segs := strings.Split(rel, "/")
	d := DanglingLink{Path: path, Target: target, relPath: rel, Name: segs[len(segs)-1]}
	if len(segs) >= 1 {
		d.Lane = segs[0]
	}
	if len(segs) >= 3 {
		d.Status = segs[len(segs)-2] // the symlink's immediate parent (bucket) dir
	}
	return d
}

// classifyBrokenLink decides, for a broken (dangling) symlink, whether it is an entry-level
// task-FILE placement (eligible for repair-or-reap) or a directory / inbox ALIAS (reported
// as broken-dir-alias, NEVER auto-reaped). A dangling target cannot be Stat'd to read its
// type, so classification is STRUCTURAL and deliberately CONSERVATIVE — anything not
// unambiguously a task-file placement is a dir alias (favouring never-reap, since dropping a
// dir alias is the exact harm we guard against):
//   - the symlink's OWN name is a canonical bucket (pending/in-progress/done): a bucket-level
//     inbox alias (e.g. cr-1-2/pending → cr-2/pending) → dir alias.
//   - the symlink's own name does NOT end in ".md", OR its target basename does NOT end in
//     ".md": a lane/dir alias whose target is a directory, not a task file → dir alias.
//
// Only a link named "<x>.md" pointing at a "<y>.md" basename is treated as a file placement.
// (The STRICT same-lane bucket-alias reap — in_progress→in-progress — is a SEPARATE pass on
// a NON-broken symlink; see reapBucketAliases. It is the only auto-removed alias and is
// untouched here.)
func classifyBrokenLink(d DanglingLink) brokenKind {
	if isBucketName(d.Name) {
		return brokenDirAlias
	}
	if !strings.HasSuffix(d.Name, ".md") {
		return brokenDirAlias
	}
	if !strings.HasSuffix(filepath.Base(d.Target), ".md") {
		return brokenDirAlias
	}
	return brokenFilePlacement
}

// planRepoint computes the correct RELATIVE symlink target from the broken link's own
// directory to the live store file <storeRoot>/<basename>. It mutates nothing — it only
// builds the plan a dry-run reports and applyRepoint later writes atomically.
func planRepoint(d DanglingLink, storeRoot, basename string) RepointRepair {
	rep := RepointRepair{Link: d, OldTarget: d.Target, StorePath: filepath.Join(storeRoot, basename)}
	linkDir := filepath.Dir(d.Path)
	if abs, err := filepath.Abs(rep.StorePath); err == nil {
		rep.StorePath = abs
	}
	// Resolved spellings on both sides, so the re-pointed link resolves where the link really is.
	rep.NewTarget = RelativeLinkTarget(linkDir, rep.StorePath)
	return rep
}

// applyRepoint performs the ATOMIC re-point (apply only): create the new symlink at a temp
// name in the SAME dir (<name>.repair-tmp), verify it RESOLVES (os.Stat succeeds), then
// os.Rename it OVER the old link (atomic replace on one filesystem). It NEVER
// os.Remove-then-Symlink — a failure between those would DROP the placement (the exact data
// loss we prevent). On ANY failure it removes the temp it created and marks the repair
// Failed, leaving the ORIGINAL link EXACTLY as it was — never a missing placement.
func applyRepoint(rep *RepointRepair) {
	linkDir := filepath.Dir(rep.Link.Path)
	tmp := filepath.Join(linkDir, rep.Link.Name+".repair-tmp")
	// Clear a STALE temp symlink from a prior aborted run — but ONLY a symlink, never a real
	// file/dir (os.Remove refuses a non-empty dir; a lone stale symlink clears). A non-symlink
	// squatting the temp name is left in place, so os.Symlink below fails safely (original untouched).
	if fi, lerr := os.Lstat(tmp); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmp)
	}
	if err := os.Symlink(rep.NewTarget, tmp); err != nil {
		rep.Failed = true
		rep.Err = fmt.Sprintf("create temp symlink: %v", err)
		return // nothing we created; ORIGINAL link untouched
	}
	if _, err := os.Stat(tmp); err != nil {
		// The new link does not RESOLVE — remove the temp, leave the original alone.
		rep.Failed = true
		rep.Err = fmt.Sprintf("verify temp symlink resolves: %v", err)
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, rep.Link.Path); err != nil {
		rep.Failed = true
		rep.Err = fmt.Sprintf("atomic rename over link: %v", err)
		_ = os.Remove(tmp)
		return
	}
	rep.Applied = true
}

// Repair is the REPAIR-FIRST reaper (docs/build-tasks/reaper-repair-first.md). It finds the
// broken symlinks under root and classifies each, using storeRoot (the FLAT STORE ROOT) to
// tell a mispointed-but-LIVE link from a true orphan:
//
//   - a broken DIRECTORY / inbox alias → REPORTED (broken-dir-alias), NEVER auto-reaped;
//   - a broken FILE link whose target basename resolves to a real file under storeRoot →
//     REPAIRABLE: the task is ALIVE, the link merely mispointed → RE-POINT it (atomic
//     temp+rename) to the correct relative path, NEVER reap;
//   - a broken FILE link whose basename is ABSENT from storeRoot → ORPHANED → reaped (apply);
//   - if storeRoot is unknown OR unreadable → REAP NOTHING; every broken FILE link is
//     reported UNVERIFIABLE (fail-safe: never delete a link we cannot prove is orphaned).
//
// In dry-run (apply=false) it mutates NOTHING and only reports the full plan. Under apply a
// re-point is ATOMIC (the placement is never momentarily missing) and a reap is a plain
// os.Remove of a confirmed-orphan symlink (its store file is already gone).
//
// pruneEmpty is the opt-in (`--prune-empty-lanes`) second pass — empty UNREGISTERED lane
// dirs + recursively-empty stray non-lane dirs + the STRICT same-lane bucket-alias reap. It
// is unchanged by this fix and runs AFTER the reap/repair.
func Repair(root, storeRoot string, apply, pruneEmpty bool) (RepairResult, error) {
	return repair(root, storeRoot, apply, pruneEmpty, true)
}

// RepairDanglingOnly is Repair for an unattended caller (the sweep's `self_repair: auto`). It does what
// that mode promises, and only that: re-point or reap dangling links. Repairing a link restores intent;
// deleting even an empty directory destroys state, and the two are different risks, so strays are
// neither listed nor cleared here. The sweep reports them, and clearing stays an explicit
// `leadkit queue repair --apply` run by a person or the lead.
func RepairDanglingOnly(root, storeRoot string) (RepairResult, error) {
	return repair(root, storeRoot, true, false, false)
}

func repair(root, storeRoot string, apply, pruneEmpty, clearStrays bool) (RepairResult, error) {
	dangling, err := FindDangling(root)
	if err != nil {
		return RepairResult{}, err
	}
	res := RepairResult{Dangling: dangling, DryRun: !apply, StoreRoot: storeRoot}

	// The store root must be DETERMINED and READABLE to tell a repairable (task-alive) link
	// from a true orphan. If it is not, we REAP NOTHING and report every broken FILE link
	// UNVERIFIABLE — fail-safe: never delete a link we cannot prove is orphaned.
	if storeRoot != "" {
		if _, serr := os.ReadDir(storeRoot); serr == nil {
			res.StoreReadable = true
		}
	}

	for _, d := range dangling {
		// 1. A directory / inbox alias is NEVER auto-reaped — report only.
		if classifyBrokenLink(d) == brokenDirAlias {
			res.DirAliases = append(res.DirAliases, d)
			continue
		}
		// 2. A file placement whose designated store we cannot read → UNVERIFIABLE (never reaped).
		if !res.StoreReadable {
			res.Unverifiable = append(res.Unverifiable, d)
			continue
		}
		base := filepath.Base(d.Target)
		// 3a. The task is ALIVE in the DESIGNATED store (basename match within storeRoot ONLY)
		//     → REPAIR (re-point), NEVER reap.
		if fi, serr := os.Stat(filepath.Join(storeRoot, base)); serr == nil && !fi.IsDir() {
			rep := planRepoint(d, storeRoot, base)
			if apply {
				applyRepoint(&rep)
			}
			res.Repairs = append(res.Repairs, rep)
			continue
		}
		// 3b. Genuinely dead (no live task in store) → ORPHAN → reap.
		res.Orphans = append(res.Orphans, d)
		if apply {
			if rerr := os.Remove(d.Path); rerr != nil {
				return res, fmt.Errorf("reap %s: %w", d.Rel(), rerr)
			}
			res.Reaped = append(res.Reaped, d)
		}
	}

	var strays []StrayEntry
	if clearStrays {
		var serr error
		if strays, serr = FindStrays(root); serr != nil {
			return res, serr
		}
	}
	res.Strays = strays
	if apply {
		for _, st := range strays {
			if !st.Removable {
				continue
			}
			if rerr := removeStray(st); rerr != nil {
				res.StrayErrors = append(res.StrayErrors, fmt.Sprintf("%s: %v", st.Rel(), rerr))
				continue
			}
			res.ClearedStrays = append(res.ClearedStrays, st)
		}
	}

	if pruneEmpty {
		res.Prune = true
		// Reap redundant bucket-alias symlinks BEFORE the empty-lane check so a lane
		// cleared of its alias becomes prune-eligible on the SAME pass.
		reapBucketAliases(root, apply, &res)
		pruneEmptyLanes(root, apply, &res)
	}
	return res, nil
}

// registeredLanes reads <root>/ROLES and returns the set of names that count as a
// "registered" lane — every field of every role→live row (both the canonical role
// name AND the live lane name), so a lane matching EITHER column is protected from the
// prune. A missing/unreadable ROLES file just yields an empty set (nothing registered),
// never an error — ROLES is not required for the queue to function.
func registeredLanes(root string) map[string]bool {
	reg := map[string]bool{}
	for _, e := range roles.List(filepath.Join(root, "ROLES")) {
		reg[e.Role] = true
		reg[e.Live] = true
	}
	return reg
}

// isLaneShaped reports whether dir holds all three status buckets as REAL directories.
// Lstat (not Stat) so a symlinked bucket — an alias inbox — disqualifies the lane from
// the prune's clean-lane path entirely (it then falls to stray handling, which only
// removes a recursively-empty dir and never dereferences a symlink).
func isLaneShaped(dir string) bool {
	for _, b := range bucketDirs {
		fi, err := os.Lstat(filepath.Join(dir, b))
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// dirEmpty reports whether dir has ZERO directory entries — dotfiles included, so any
// content at all (a symlink, a file, a hidden file) makes it non-empty. An unreadable
// dir is treated as non-empty (conservative: never a prune candidate).
func dirEmpty(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(ents) == 0
}

// laneAllEmpty reports whether every one of a lane-shaped dir's three buckets is empty.
func laneAllEmpty(dir string) bool {
	for _, b := range bucketDirs {
		if !dirEmpty(filepath.Join(dir, b)) {
			return false
		}
	}
	return true
}

// recursivelyEmpty reports whether dir contains NO file (or symlink) at any depth — it
// may hold nested empty dirs, nothing else. Any regular file, symlink, or unreadable
// entry makes it non-empty. This is the sole gate for removing a stray dir.
func recursivelyEmpty(dir string) bool {
	empty := true
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			empty = false
			return filepath.SkipAll
		}
		if p == dir {
			return nil
		}
		if !d.IsDir() { // a file, a symlink — anything that isn't a plain dir
			empty = false
			return filepath.SkipAll
		}
		return nil
	})
	return empty
}

// removeEmptyTree removes dir and every subdir it contains, deepest-first, using ONLY
// os.Remove (which fails on a non-empty dir) — NEVER a recursive force-delete. It first
// refuses if it encounters any non-dir entry, so a tree holding a file (or a symlink)
// is never touched. Callers gate this behind recursivelyEmpty / laneAllEmpty, so this
// is belt-and-braces against a concurrent write racing in between.
func removeEmptyTree(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return fmt.Errorf("refusing to prune %s: it holds %s", dir, p)
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first
		if err := os.Remove(dirs[i]); err != nil { // fails if (racily) non-empty
			return err
		}
	}
	return nil
}

// pruneEmptyLanes is the opt-in `--prune-empty-lanes` pass. It walks the DIRECT children
// of the queue root and, conservatively:
//   - an empty lane-shaped dir (all three buckets empty) NOT in <root>/ROLES is a prune
//     candidate — reported in dry-run, removed with apply;
//   - an empty lane-shaped dir that IS in ROLES is reported skipped-registered and NEVER
//     removed (a registered role lane legitimately sits empty between work);
//   - a lane with any entry in any bucket is never a candidate (not touched);
//   - a stray non-lane dir (not lane-shaped) is reported, and removed ONLY when it is
//     recursively empty AND apply is set — a stray holding any file is reported, never
//     removed.
//
// It NEVER touches the flat tasks/ store (skipped by name) or the ROLES file, only ever
// removes an EMPTY dir, and logs (never aborts on) a removal failure.
func pruneEmptyLanes(root string, apply bool, res *RepairResult) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	reg := registeredLanes(root)
	for _, e := range entries {
		name := e.Name()
		// Never the ROLES routing file nor the flat canonical store — overlay dirs only.
		if name == "ROLES" || name == "tasks" || !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, name)
		// A symlinked dir at the root (an alias) is left strictly alone.
		if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if isLaneShaped(dir) {
			if !laneAllEmpty(dir) {
				continue // has work in some bucket — never a candidate
			}
			el := EmptyLane{Lane: name, Path: dir, Registered: reg[name]}
			res.EmptyLanes = append(res.EmptyLanes, el)
			if apply && !el.Registered {
				if rerr := removeEmptyTree(dir); rerr != nil {
					res.PruneErrors = append(res.PruneErrors, fmt.Sprintf("prune lane %s: %v", name, rerr))
				} else {
					res.PrunedLanes = append(res.PrunedLanes, el)
				}
			}
			continue
		}
		// Not lane-shaped → a stray structural artifact.
		sd := StrayDir{Lane: name, Path: dir, Empty: recursivelyEmpty(dir)}
		res.StrayDirs = append(res.StrayDirs, sd)
		if apply && sd.Empty {
			if rerr := removeEmptyTree(dir); rerr != nil {
				res.PruneErrors = append(res.PruneErrors, fmt.Sprintf("prune stray %s: %v", name, rerr))
			} else {
				res.PrunedStray = append(res.PrunedStray, sd)
			}
		}
	}
	sort.Slice(res.EmptyLanes, func(i, j int) bool { return res.EmptyLanes[i].Lane < res.EmptyLanes[j].Lane })
	sort.Slice(res.StrayDirs, func(i, j int) bool { return res.StrayDirs[i].Lane < res.StrayDirs[j].Lane })
}

// isBucketName reports whether name is one of the canonical (hyphen) status buckets.
func isBucketName(name string) bool {
	for _, b := range bucketDirs {
		if name == b {
			return true
		}
	}
	return false
}

// classifyBucketAlias inspects a single bucket-level entry `entryName` directly inside
// `laneDir`. It returns (alias, candidate, reapable):
//
//   - candidate=true only when the entry is a SYMLINK whose NAME is a separator-variant
//     of a canonical bucket — i.e. replacing '_' with '-' yields a known bucket and the
//     name is NOT already that canonical bucket (so `in_progress` qualifies; the real
//     `pending`/`in-progress`/`done` dirs, and a real alias-inbox symlink NAMED after a
//     canonical bucket like `pending`, never do). This is the ONLY shape the pass ever
//     considers — everything else is left strictly alone, not even reported.
//   - reapable=true only when, additionally, the canonical sibling bucket exists as a
//     REAL dir in the SAME lane AND the alias's target resolves EXACTLY to that sibling.
//     A target that resolves elsewhere (a different lane, a real inbox), a missing/
//     symlinked sibling, or a dangling target ⇒ reapable=false: reported, never removed.
func classifyBucketAlias(laneDir, laneName, entryName string) (BucketAlias, bool, bool) {
	canon := strings.ReplaceAll(entryName, "_", "-")
	if canon == entryName || !isBucketName(canon) {
		return BucketAlias{}, false, false // not a separator-variant of a bucket name
	}
	p := filepath.Join(laneDir, entryName)
	fi, lerr := os.Lstat(p)
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		return BucketAlias{}, false, false // not a symlink — not an alias candidate
	}
	target, _ := os.Readlink(p)
	alias := BucketAlias{Lane: laneName, Name: entryName, Canon: canon, Path: p, Target: target}

	// The canonical sibling bucket must exist as a REAL (non-symlink) dir in this lane.
	sibling := filepath.Join(laneDir, canon)
	sfi, serr := os.Lstat(sibling)
	if serr != nil || !sfi.IsDir() || sfi.Mode()&os.ModeSymlink != 0 {
		return alias, true, false // candidate, but no real same-lane sibling → left alone
	}
	// The alias's target must resolve EXACTLY to that same-lane canonical sibling.
	targetReal, terr := filepath.EvalSymlinks(p)
	if terr != nil {
		return alias, true, false // dangling / unresolvable target → left alone
	}
	siblingReal, srerr := filepath.EvalSymlinks(sibling)
	if srerr != nil || targetReal != siblingReal {
		return alias, true, false // points somewhere else (e.g. a different lane) → left alone
	}
	return alias, true, true // unambiguous same-lane redundant bucket alias
}

// reapBucketAliases is the alias-reap step of the opt-in --prune-empty-lanes pass. It
// walks the DIRECT children (lanes) of the queue root and, for each, examines the
// bucket-level entries for a redundant separator-variant bucket alias (see
// classifyBucketAlias). It is STRICT: it removes (apply) ONLY an unambiguous same-lane
// alias in an UNREGISTERED lane — removing a symlink never touches its target. Every other
// candidate (different-lane target, sibling missing, or an alias in a REGISTERED lane) is
// reported with Reap=false and left on disk. It never dereferences or descends anything,
// never touches ROLES or the flat tasks/ store, and logs (never aborts on) a removal error.
func reapBucketAliases(root string, apply bool, res *RepairResult) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	reg := registeredLanes(root)
	for _, e := range entries {
		name := e.Name()
		if name == "ROLES" || name == "tasks" || !e.IsDir() {
			continue
		}
		laneDir := filepath.Join(root, name)
		// A symlinked lane dir at the root is an alias — left strictly alone (mirror prune).
		if fi, lerr := os.Lstat(laneDir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		laneEntries, lerr := os.ReadDir(laneDir)
		if lerr != nil {
			continue
		}
		for _, le := range laneEntries {
			alias, candidate, reapable := classifyBucketAlias(laneDir, name, le.Name())
			if !candidate {
				continue
			}
			// Reap ONLY an unambiguous same-lane alias in an UNREGISTERED lane.
			alias.Reap = reapable && !reg[name]
			res.BucketAliases = append(res.BucketAliases, alias)
			if apply && alias.Reap {
				if rerr := os.Remove(alias.Path); rerr != nil {
					res.PruneErrors = append(res.PruneErrors, fmt.Sprintf("reap alias %s/%s: %v", alias.Lane, alias.Name, rerr))
				} else {
					res.ReapedAliases = append(res.ReapedAliases, alias)
				}
			}
		}
	}
	sort.Slice(res.BucketAliases, func(i, j int) bool {
		return res.BucketAliases[i].Lane+"/"+res.BucketAliases[i].Name < res.BucketAliases[j].Lane+"/"+res.BucketAliases[j].Name
	})
}

// FormatRepair renders a RepairResult as the human report `leadkit queue repair`
// prints and, in auto mode, the sweep logs — one line per link plus a summary.
func FormatRepair(res RepairResult) string {
	var b strings.Builder

	// REPAIR: mispointed-but-live links re-pointed (never reaped).
	for _, r := range res.Repairs {
		switch {
		case res.DryRun:
			fmt.Fprintf(&b, "  REPAIR %s (%s → %s) — task alive in store; re-point (not reaped)\n", r.Rel(), r.OldTarget, r.NewTarget)
		case r.Applied:
			fmt.Fprintf(&b, "  REPAIRED %s (%s → %s)\n", r.Rel(), r.OldTarget, r.NewTarget)
		default: // -apply, but the atomic re-point failed → original left in place
			fmt.Fprintf(&b, "  repair-failed %s (%s → %s): %s — ORIGINAL link left in place (no missing placement)\n", r.Rel(), r.OldTarget, r.NewTarget, r.Err)
		}
	}

	// REAP: true orphans (no live task in store).
	reapVerb := "reaped"
	if res.DryRun {
		reapVerb = "REAP"
	}
	for _, d := range res.Orphans {
		fmt.Fprintf(&b, "  %s %s (orphaned — dangling → %s; no live task in store)\n", reapVerb, d.Rel(), d.Target)
	}

	// broken-dir-alias: reported, NEVER auto-reaped.
	for _, d := range res.DirAliases {
		fmt.Fprintf(&b, "  broken-dir-alias %s (dangling → %s) — reported, NOT auto-reaped\n", d.Rel(), d.Target)
	}

	// UNVERIFIABLE: store root unknown/unreadable → nothing reaped.
	for _, d := range res.Unverifiable {
		fmt.Fprintf(&b, "  UNVERIFIABLE %s (dangling → %s) — store root unknown; not reaped\n", d.Rel(), d.Target)
	}

	if len(res.Dangling) == 0 {
		b.WriteString("  no dangling links found — queue clean\n")
	}

	repairFailed := 0
	for _, r := range res.Repairs {
		if r.Failed {
			repairFailed++
		}
	}
	if res.DryRun {
		fmt.Fprintf(&b, "queue-repair: plan — REPAIR %d, REAP %d, broken-dir-alias %d (reported), UNVERIFIABLE %d (dry-run — nothing changed; re-run with --apply)\n",
			len(res.Repairs), len(res.Orphans), len(res.DirAliases), len(res.Unverifiable))
	} else {
		fmt.Fprintf(&b, "queue-repair: repaired %d, reaped %d; broken-dir-alias %d (reported), UNVERIFIABLE %d",
			len(res.Repairs)-repairFailed, len(res.Reaped), len(res.DirAliases), len(res.Unverifiable))
		if repairFailed > 0 {
			fmt.Fprintf(&b, "; %d repair(s) FAILED (original links left in place)", repairFailed)
		}
		b.WriteString("\n")
	}
	b.WriteString(formatStrays(res))
	if res.Prune {
		b.WriteString(formatPrune(res))
	}
	return b.String()
}

// formatStrays renders the non-ticket entries found in pending/ and in-progress/: one line each, and what
// was or would be done. Nothing is printed when there are none, so a clean queue's report is unchanged.
func formatStrays(res RepairResult) string {
	var b strings.Builder
	cleared := map[string]bool{}
	for _, st := range res.ClearedStrays {
		cleared[st.Rel()] = true
	}
	removable := 0
	for _, st := range res.Strays {
		switch {
		case cleared[st.Rel()]:
			fmt.Fprintf(&b, "  cleared stray %s (%s)\n", st.Rel(), st.describe())
		case st.Removable && res.DryRun:
			removable++
			fmt.Fprintf(&b, "  CLEAR stray %s (%s)\n", st.Rel(), st.describe())
		case st.Removable:
			removable++
			fmt.Fprintf(&b, "  stray %s (%s) — clearing failed, left in place\n", st.Rel(), st.describe())
		default:
			fmt.Fprintf(&b, "  stray %s (%s) — reported, NOT removed\n", st.Rel(), st.describe())
		}
	}
	for _, e := range res.StrayErrors {
		fmt.Fprintf(&b, "  stray-error: %s\n", e)
	}
	if len(res.Strays) > 0 {
		if res.DryRun {
			fmt.Fprintf(&b, "queue-repair: %d stray non-ticket entr(ies), %d clearable (dry-run — nothing changed; re-run with --apply)\n", len(res.Strays), removable)
		} else {
			fmt.Fprintf(&b, "queue-repair: %d stray non-ticket entr(ies); cleared %d\n", len(res.Strays), len(res.ClearedStrays))
		}
	}
	return b.String()
}

// formatPrune renders the opt-in --prune-empty-lanes section: empty lanes (removed /
// would remove / skipped-registered), stray dirs (removed / would remove / reported),
// and any best-effort removal errors, plus a summary line.
func formatPrune(res RepairResult) string {
	var b strings.Builder
	laneVerb, strayVerb, aliasVerb := "removed", "removed", "reaped"
	if res.DryRun {
		laneVerb, strayVerb, aliasVerb = "would remove", "would remove", "would reap"
	}
	for _, a := range res.BucketAliases {
		if a.Reap {
			fmt.Fprintf(&b, "  %s %s/%s (redundant bucket alias → %s — same-lane separator variant)\n", aliasVerb, a.Lane, a.Name, a.Canon)
		} else {
			fmt.Fprintf(&b, "  reported %s/%s (bucket-alias-shaped symlink → %s — NOT an unambiguous same-lane alias / registered lane; left alone)\n", a.Lane, a.Name, a.Target)
		}
	}
	for _, l := range res.EmptyLanes {
		if l.Registered {
			fmt.Fprintf(&b, "  skipped %s (empty but registered in ROLES — never auto-pruned)\n", l.Lane)
			continue
		}
		fmt.Fprintf(&b, "  %s %s/ (empty unregistered lane)\n", laneVerb, l.Lane)
	}
	for _, s := range res.StrayDirs {
		if s.Empty {
			fmt.Fprintf(&b, "  %s %s/ (empty stray non-lane dir)\n", strayVerb, s.Lane)
		} else {
			fmt.Fprintf(&b, "  reported %s/ (stray non-lane dir HOLDS files — not removed; inspect by hand)\n", s.Lane)
		}
	}
	for _, e := range res.PruneErrors {
		fmt.Fprintf(&b, "  prune-error: %s\n", e)
	}
	candidates := 0
	for _, l := range res.EmptyLanes {
		if !l.Registered {
			candidates++
		}
	}
	strayRemovable := 0
	for _, s := range res.StrayDirs {
		if s.Empty {
			strayRemovable++
		}
	}
	aliasReapable := 0
	for _, a := range res.BucketAliases {
		if a.Reap {
			aliasReapable++
		}
	}
	if res.DryRun {
		fmt.Fprintf(&b, "queue-repair: prune dry-run — %d redundant bucket-alias(es) + %d empty unregistered lane(s) + %d empty stray dir(s) would be removed (re-run with --apply)\n", aliasReapable, candidates, strayRemovable)
	} else {
		fmt.Fprintf(&b, "queue-repair: reaped %d redundant bucket-alias(es); pruned %d empty lane(s) + %d empty stray dir(s)\n", len(res.ReapedAliases), len(res.PrunedLanes), len(res.PrunedStray))
	}
	return b.String()
}
