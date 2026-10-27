// Package queue folds the filesystem work-queue shell scripts (queue-move.sh,
// queue-sweep.sh, queue-wait.sh, queue-waker.sh) into leadkit.
//
// The queue is a symlink overlay onto a flat canonical task store: each entry under
// <root>/<agent>/<status>/ is a symlink into tasks/, and the *directory* encodes the
// agent (lane) + status. A transition = relinking (moving) the symlink between status
// dirs — never touching the store file. These ops preserve that model exactly, and are
// deliberately confined to the queue root so a `move` can never touch anything else.
package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultRoot resolves the queue root when no leadkit.yaml overrides queue_root:
// $LEADKIT_QUEUE if set, else a cwd-relative ./queue (<cwd>/queue). No adopter path
// is baked in — a project's own leadkit.yaml (or the env var) points it wherever the
// project's queue actually lives.
func DefaultRoot() string {
	if q := os.Getenv("LEADKIT_QUEUE"); q != "" {
		return q
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "queue")
	}
	return "queue"
}

// resolveInRoot returns the absolute path of p with its ancestor dirs' symlinks
// resolved but the leaf left untouched (so a symlink entry is not dereferenced), and
// reports whether it lies inside root. Mirrors queue-move.sh's `cd dirname &&
// pwd`/basename.
func resolveInRoot(root, p string) (string, bool, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, fmt.Errorf("queue root unresolvable: %w", err)
	}
	full, err := resolveLeafPreserving(p)
	if err != nil {
		return "", false, err
	}
	return full, strings.HasPrefix(full, rootReal+string(os.PathSeparator)), nil
}

// resolveLeafPreserving resolves symlinks along p's ancestor dirs — walking up past
// not-yet-created status dirs (a fresh status dir doesn't exist until Move creates it)
// to the nearest existing ancestor — then rejoins the unresolved suffix and the leaf
// name verbatim, so the leaf itself (typically the queue entry's symlink) is never
// dereferenced. Without walking up, a not-yet-existing parent left root's queue-root
// symlinks (e.g. macOS's /var -> /private/var, which is what t.TempDir() returns)
// resolved on one side of the range check and not the other, so an in-root dest could
// be wrongly refused as outside root.
func resolveLeafPreserving(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	leaf := filepath.Base(abs)
	dir := filepath.Dir(abs)
	var suffix []string
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			parts := append([]string{real}, suffix...)
			return filepath.Join(append(parts, leaf)...), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding an existing ancestor;
			// nothing left to resolve.
			parts := append([]string{dir}, suffix...)
			return filepath.Join(append(parts, leaf)...), nil
		}
		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent
	}
}

// Move is the restricted mv wrapper (queue-move.sh): both endpoints must resolve inside
// the queue root, the dest dir is created, then the entry (typically a symlink) is moved.
// Covers every transition: claim, advance, bounce-back, done — same primitive throughout.
// dryRun runs every refusal check unchanged but skips MkdirAll/Rename, printing what
// WOULD happen instead — mutates nothing.
func Move(root, from, to string, dryRun bool) error {
	if strings.TrimSpace(from) == "" {
		return fmt.Errorf("refuse — source is empty: a ticket name that resolved to nothing")
	}
	if _, err := os.Lstat(from); err != nil {
		return fmt.Errorf("refuse — source does not exist: %s", from)
	}
	// A ticket is a file, or a symlink to one. A directory source is what an EMPTY ticket name resolves
	// to (<lane>/in-progress/ + "" is the state directory itself), and moving it relocates a whole state
	// directory, which is how a lane ended up with a stray `in-progress` inside its pending.
	if fi, err := os.Stat(from); err == nil && fi.IsDir() {
		return fmt.Errorf("refuse — source is a directory: %s (queue move relocates tickets, never a lane or state directory; an empty ticket name resolves to one)", from)
	}
	// Dir-form dest: a `to` ending in a path separator, or one that resolves to an
	// existing directory, is a DESTINATION DIRECTORY — append the src's basename
	// (mirroring `mv`'s dir-dest convention: `queue/qa/pending/` + `1454-x.md` →
	// `queue/qa/pending/1454-x.md`) rather than relinking AT the dir path (which would
	// error "file exists" against the dir). A plain (non-dir, non-trailing-slash) path
	// keeps today's exact-name behaviour, so nothing regresses.
	if isDirDest(to) {
		to = filepath.Join(to, filepath.Base(from))
	}
	fromReal, ok, err := resolveInRoot(root, from)
	if err != nil {
		return err
	}
	if !ok {
		// Names the RESOLVED queue root next to the offending path — a CWD-relative
		// `from`/`to` that happens to look like it's inside the queue (but resolves
		// against the wrong root, e.g. the caller's cwd differs from what --queue/
		// the profile actually resolved to) is otherwise a confusing "outside queue
		// root" refusal with no visible mismatch to point at.
		return fmt.Errorf("refuse — source outside queue root: resolved queue root = %s; source %s is outside it", root, fromReal)
	}
	toReal, ok, err := resolveInRoot(root, to)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("refuse — dest outside queue root: resolved queue root = %s; dest %s is outside it", root, toReal)
	}
	// The destination is <lane>/<state>/<entry>. One directly under a lane has named the lane where a
	// state belongs, which parks the ticket where no verb looks for it.
	if rel, err := filepath.Rel(rootRealOr(root), toReal); err == nil && len(strings.Split(filepath.ToSlash(rel), "/")) == 2 {
		return fmt.Errorf("refuse — dest %s is directly under a lane: name a state inside it (e.g. %s/pending/)", toReal, filepath.Dir(toReal))
	}
	// The destination must be <lane>/<state>/<ticket> with a CANONICAL state. Anything else
	// strands the ticket: Wait watches only pending and in-progress, so no owner is ever woken
	// for a ticket moved to, say, `active/`, and the lane looks idle while holding work. This
	// is checked on the resolved dest, after the dir-form append above.
	if err := checkDestState(root, toReal); err != nil {
		return err
	}
	// A blocked ticket may not be claimed. `blocked` is a recorded status, and a claim is the move
	// into in-progress; without this check the only thing between an owner and a ticket that must
	// wait is a sentence in its prose. Fail closed, and say why and what to do about it.
	if filepath.Base(filepath.Dir(toReal)) == "in-progress" {
		if blocked, on := ticketBlocked(from); blocked {
			return fmt.Errorf("refuse — %s is status: blocked%s. A blocked ticket is not claimable. Work it deliberately by changing its status once it is unblocked, "+
				"or take it out of the lane with `leadkit queue unlink <entry> -allow-last -apply`",
				filepath.Base(from), blockedOnClause(on))
		}
	}
	// Never clobber an existing entry: if the resolved dest already exists (and is not
	// the source itself — a dir-dest that resolves back onto the source is a harmless
	// self-move), refuse rather than overwrite it. Confined-and-safe: the dir-dest
	// append above can only ever land a new name inside a dir already proven in-root.
	if fromReal != toReal {
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("refuse — dest already exists: %s (queue move never clobbers an existing entry)", toReal)
		}
	}
	if dryRun {
		fmt.Printf("DRY RUN: queue-move: %s -> %s (would relink, nothing done)\n", fromReal, toReal)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	// A claim describes a ticket's stay in one in-progress. Leaving it ends the claim, and arriving
	// starts an unclaimed stay: a sidecar left by an earlier occupant must not be read as the new one's.
	if fromReal != toReal {
		if inProgressDir(from) {
			clearClaim(from)
		}
		if inProgressDir(to) {
			clearClaim(to)
		}
	}
	fmt.Printf("queue-move: %s -> %s\n", fromReal, toReal)
	return nil
}

// rootRealOr is the queue root with symlinks resolved, as resolveInRoot judges it; the root itself when
// it cannot be resolved (resolveInRoot has already refused that case by the time this is called).
func rootRealOr(root string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		return r
	}
	return root
}

func blockedOnClause(on string) string {
	if on == "" {
		return " (no blocked_on recorded)"
	}
	return " (blocked_on: " + on + ")"
}

// claimablePending lists a lane's pending entries that an owner may actually claim: a ticket
// that is `status: blocked` is not work, a ticket already in a terminal status has nothing left to
// do, and neither is an entry that is not a ticket, so a lane holding only those reads as idle
// instead of waking an owner that then has nothing it may take.
func claimablePending(pendDir string) []string {
	return actionable(pendDir)
}

// actionable is the entries of dir that are tickets AND still wake an owner: neither blocked
// nor in a terminal status. In in-progress a blocked or terminal ticket is a leftover claim, and
// reporting it as an orphan would wake every owner on the lane forever. An unreadable or status-less ticket stays actionable, the conservative
// default, because waking for nothing costs a look and sleeping through real work costs a lane.
func actionable(dir string) []string {
	var out []string
	for _, name := range listTickets(dir) {
		p := filepath.Join(dir, name)
		if blocked, _ := ticketBlocked(p); blocked {
			continue
		}
		if terminal, _ := ticketTerminal(p); terminal {
			continue
		}
		out = append(out, name)
	}
	return out
}

// checkDestState refuses a destination that is not <lane>/<state>/<ticket> under root with state
// one of the canonical status buckets (pending, in-progress, done).
func checkDestState(root, toReal string) error {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("queue root unresolvable: %w", err)
	}
	rel, err := filepath.Rel(rootReal, toReal)
	if err != nil {
		return fmt.Errorf("refuse — dest %s is not under the queue root", toReal)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 {
		return fmt.Errorf("refuse — dest %s is not <lane>/<state>/<ticket> under the queue root (valid states: %s)", toReal, strings.Join(bucketDirs, ", "))
	}
	if !isBucketName(parts[1]) {
		return fmt.Errorf("refuse — %q is not a queue state: a ticket moved to %s/%s/ is stranded, because nothing watches that directory (valid states: %s)",
			parts[1], parts[0], parts[1], strings.Join(bucketDirs, ", "))
	}
	return nil
}

// isDirDest reports whether `to` should be treated as a destination DIRECTORY (so
// Move appends the src basename): true when it ends in a path separator, or when it
// already exists as a directory. os.Stat (not Lstat) so a symlink-to-dir counts too.
func isDirDest(to string) bool {
	if strings.HasSuffix(to, string(os.PathSeparator)) || strings.HasSuffix(to, "/") {
		return true
	}
	if fi, err := os.Stat(to); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// listEntries returns non-dotfile names directly under dir (like the shell glob).
func listEntries(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// StalledLane is one lane that has resolvable pending tickets but nothing
// in-progress — structured detail feeding the machine-readable findings artifact
// (sweep-status.json) and the self-surfacing diagnostics, alongside the human Lines.
type StalledLane struct {
	Lane    string   `json:"lane"`
	Pending int      `json:"pending"`
	Entries []string `json:"entries,omitempty"`
}

// BrokenLane is one lane carrying dangling queue symlinks (target gone) — the
// structured detail `leadkit queue repair` reaps and the artifact records.
type BrokenLane struct {
	Lane  string   `json:"lane"`
	Links []string `json:"links"`
}

// LaneCount is one lane's pending/in-progress ticket counts exactly as the sweep's
// single pass observed them — recorded for EVERY real lane it visits (not only the
// stalled ones StalledLanes captures). The owner-liveness doctor check and `leadkit
// team standup` consume this so they never re-walk or re-count the queue: the
// per-lane liveness numbers already computed here are the single source (the spec's
// "reuse Sweep's per-lane counts, don't fork").
type LaneCount struct {
	Lane       string `json:"lane"`
	Pending    int    `json:"pending"`
	InProgress int    `json:"in_progress"`
}

// InProgressClaim is one CLAIMED in-progress ticket and when it was claimed — the
// claim age is the in-progress symlink's mtime (os.Lstat, NOT dereferenced: the
// symlink itself is the claim marker, bumped when the ticket was moved in). The
// frozen-claim detector (docs/build-tasks/frozen-claim-and-noninteractive.md)
// correlates ClaimedAt against the project's deliverable clock to catch a hung owner
// holding a ticket that isn't advancing — reusing THIS census, never re-walking
// in-progress. ClaimedOK=false records that the mtime could not be read (a broken
// stat), so the detector degrades that ticket to Info rather than a false Warn.
type InProgressClaim struct {
	Lane      string    `json:"lane"`
	Ticket    string    `json:"ticket"`
	ClaimedAt time.Time `json:"claimed_at"`
	ClaimedOK bool      `json:"claimed_ok"`
}

// BlockedTicket is a pending ticket whose status is blocked: expected, not broken, so it is
// REPORTED but never counted as stalled work and never fails -strict. Excluding blocked tickets from
// wake and from the stalled count (so an owner is not woken for work it may not take) must not make
// them invisible, or one that is forgotten, or whose blocker has cleared, sits unreported for ever.
type BlockedTicket struct {
	Lane      string `json:"lane"`
	Ticket    string `json:"ticket"`
	BlockedOn string `json:"blocked_on,omitempty"`
}

// FinishedTicket is a pending ticket whose status is terminal: the work is over, so wait does not
// wake an owner for it, and it is not STALLED work either. It is still reported, because a ticket
// marked done by mistake while work remains is invisible to its lane once wait ignores it, and this
// report is the only place left that would show it.
type FinishedTicket struct {
	Lane   string `json:"lane"`
	Ticket string `json:"ticket"`
	Status string `json:"status"`
}

// StrayState is one directory inside a lane that is not a canonical state (pending,
// in-progress, done) and still holds entries. Nothing watches it, so a ticket there is
// stranded: no owner is woken for it, and the lane can read as idle while holding work.
type StrayState struct {
	Lane    string   `json:"lane"`
	Dir     string   `json:"dir"`
	Tickets []string `json:"tickets"`
}

// SweepResult is the queue-sweep.sh outcome: stalled inboxes + broken links. Lines
// is the human report; StalledLanes/BrokenLanes are the same findings structured so
// the findings artifact and every self-surfacing reader can consume them. Lanes is
// the full per-lane pending/in-progress census (see LaneCount) — every consumer of
// the "is anyone consuming this lane?" signal reads it instead of re-counting.
// InProgressClaims is the per-ticket claim census (see InProgressClaim) the
// frozen-claim detector reads.
type SweepResult struct {
	Stalled          int
	Broken           int
	Blocked          []BlockedTicket  // pending tickets that are status: blocked; reported, never stalled
	Finished         []FinishedTicket // pending tickets in a terminal status; reported, never stalled
	Stray            int              // tickets stranded in non-canonical state directories (see StrayState)
	StrayStates      []StrayState
	Lines            []string
	StalledLanes     []StalledLane
	BrokenLanes      []BrokenLane
	Lanes            []LaneCount
	InProgressClaims []InProgressClaim
	Strays           []StrayEntry // entries in pending/ or in-progress/ that are not tickets (see strays.go)
}

// Sweep is the queue health check (queue-sweep.sh). STALLED = an agent with resolvable
// pending tickets but nothing in-progress (work it has not claimed → poke it). BROKEN =
// a dangling symlink (target gone). When poke is set, each stalled inbox entry's symlink
// mtime is bumped (touch -h) to fire a watcher event for a wake that was missed.
func Sweep(root string, poke bool) (SweepResult, error) {
	var res SweepResult
	ents, err := os.ReadDir(root)
	if err != nil {
		return res, fmt.Errorf("no queue dir at %s: %w", root, err)
	}
	for _, e := range ents {
		agent := e.Name()
		if agent == "ROLES" || !e.IsDir() {
			continue
		}
		pendDir := filepath.Join(root, agent, "pending")
		// A symlinked pending/ is an ALIAS inbox (dead-queue redirect) — skip so the
		// same tickets aren't triple-counted under the alias.
		if fi, lerr := os.Lstat(pendDir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}

		var broke []string
		pend := 0
		var pendEntries []string // claimable ones only: a blocked ticket is not stalled work
		for _, name := range listEntries(pendDir) {
			p := filepath.Join(pendDir, name)
			if isWorkEntry(p) {
				if blocked, on := ticketBlocked(p); blocked {
					res.Blocked = append(res.Blocked, BlockedTicket{Lane: agent, Ticket: name, BlockedOn: on})
					res.Lines = append(res.Lines, fmt.Sprintf("  BLOCKED %s — %s%s", agent, name, blockedOnClause(on)))
					continue
				}
				if terminal, status := ticketTerminal(p); terminal {
					res.Finished = append(res.Finished, FinishedTicket{Lane: agent, Ticket: name, Status: status})
					res.Lines = append(res.Lines, fmt.Sprintf("  FINISHED %s — %s (status: %s) is still in pending; wait ignores it. If the work is done move it to done, otherwise correct its status", agent, name, status))
					continue
				}
				pendEntries = append(pendEntries, name)
				pend++
			} else if _, serr := os.Stat(p); serr != nil {
				if _, lerr := os.Lstat(p); lerr == nil {
					broke = append(broke, name) // symlink present, target gone
				}
			}
		}
		prog := 0
		for _, name := range listEntries(filepath.Join(root, agent, "in-progress")) {
			ipPath := filepath.Join(root, agent, "in-progress", name)
			if isWorkEntry(ipPath) {
				prog++
				// Record the claim age from the in-progress symlink's OWN mtime
				// (os.Lstat, not dereferenced) — the claim marker, bumped when the
				// ticket was moved in. A failed lstat records ClaimedOK=false so the
				// frozen-claim detector degrades that ticket to Info, never a false Warn.
				claim := InProgressClaim{Lane: agent, Ticket: name}
				if fi, lerr := os.Lstat(ipPath); lerr == nil {
					claim.ClaimedAt, claim.ClaimedOK = fi.ModTime(), true
				}
				res.InProgressClaims = append(res.InProgressClaims, claim)
			}
		}

		for _, ss := range strayStates(root, agent) {
			res.Stray += len(ss.Tickets)
			res.StrayStates = append(res.StrayStates, ss)
			res.Lines = append(res.Lines, fmt.Sprintf("  STRAY   %s/%s — %d ticket(s) in a directory no owner watches: %s",
				agent, ss.Dir, len(ss.Tickets), strings.Join(ss.Tickets, " ")))
		}

		// Record the per-lane census for EVERY real lane this pass reached — the
		// same pend/prog the switch below reasons over, exposed structurally so
		// owner-liveness / `team standup` don't re-count (see LaneCount).
		res.Lanes = append(res.Lanes, LaneCount{Lane: agent, Pending: pend, InProgress: prog})

		for _, st := range strayEntries(root, agent) {
			res.Strays = append(res.Strays, st)
			res.Lines = append(res.Lines, fmt.Sprintf("  STRAY   %s — %s (not a ticket, so not counted as work)", st.Rel(), st.describe()))
		}

		if len(broke) > 0 {
			res.Lines = append(res.Lines, fmt.Sprintf("  BROKEN  %s — %s", agent, strings.Join(broke, " ")))
			res.Broken++
			res.BrokenLanes = append(res.BrokenLanes, BrokenLane{Lane: agent, Links: broke})
		}
		switch {
		case pend > 0 && prog == 0:
			res.Lines = append(res.Lines, fmt.Sprintf("  STALLED %s — %d pending, 0 in-progress: %s",
				agent, pend, strings.Join(pendEntries, " ")))
			res.Stalled++
			res.StalledLanes = append(res.StalledLanes, StalledLane{Lane: agent, Pending: pend, Entries: pendEntries})
			if poke {
				for _, name := range pendEntries {
					chtimesNoDeref(filepath.Join(pendDir, name))
				}
				res.Lines = append(res.Lines, fmt.Sprintf("          poked %s's inbox", agent))
			}
		case pend > 0 || prog > 0:
			res.Lines = append(res.Lines, fmt.Sprintf("  ok      %s — %d pending, %d in-progress", agent, pend, prog))
		}
	}
	return res, nil
}

// strayStates lists the non-canonical, non-empty directories directly inside one lane. A
// separator-variant alias of a canonical bucket (`in_progress` pointing at the real
// `in-progress`) is not stray: it resolves to the same tickets the lane already counts.
func strayStates(root, lane string) []StrayState {
	laneDir := filepath.Join(root, lane)
	ents, err := os.ReadDir(laneDir)
	if err != nil {
		return nil
	}
	canonical := map[string]bool{}
	for _, b := range bucketDirs {
		if real, err := filepath.EvalSymlinks(filepath.Join(laneDir, b)); err == nil {
			canonical[real] = true
		}
	}
	var out []StrayState
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") || isBucketName(name) {
			continue
		}
		p := filepath.Join(laneDir, name)
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil && canonical[real] {
			continue
		}
		if tickets := listEntries(p); len(tickets) > 0 {
			out = append(out, StrayState{Lane: lane, Dir: name, Tickets: tickets})
		}
	}
	return out
}

// WaitResult is the queue-wait.sh outcome.
type WaitResult struct {
	Kind string // "LOCKED" | "ORPHAN-INPROGRESS" | "WORK-PENDING" | "TIMEOUT"
	Msg  string
}

// Wait blocks at zero token cost until an agent's role-queue has work, then returns.
// lockPath is the pause lock (see internal/lock) — checked FIRST each pass, ahead of
// in-progress/pending, so an agent already idle-waiting on its inbox notices a lock
// and can switch into `lock wait`, no message required. Pass "" to disable this check
// (prior behaviour, unconditionally). Once past the lock check, in-progress is checked
// before pending: a claimed-but-unfinished (orphaned) item is invisible to pending-only
// watching and would otherwise strand forever.
//
// Wait is EVENT-DRIVEN (fsnotify): after an immediate check on entry (which returns an
// already-satisfied condition instantly and closes the missed-event race — work landing
// between the caller deciding to wait and the watch being set up), it blocks on an
// fsnotify watch of the role dir's pending/ and in-progress/ subdirs AND the lock file's
// PARENT dir (fsnotify can't watch a not-yet-existing file — a Create of the lock path
// fires on its parent). A PUSHED ticket therefore wakes the WAITER the instant it lands,
// with no external wake binary. On every event the FULL lock→in-progress→pending check
// is re-run — the triggering event is never trusted alone (mirrors maybeWake's "re-check
// the inbox, don't trust the event", which is what stops a claim's removal event from
// looping). timeout still bounds the whole wait (TIMEOUT with the same re-arm Msg).
// interval is now a SAFETY-NET fallback cadence only: if fsnotify setup fails (unsupported
// fs, watch-limit hit) Wait degrades to the prior poll loop at interval, and even in the
// fsnotify path a belt-and-braces re-check fires every interval so a coalesced/dropped
// event can't strand an agent forever. Blocking on the fsnotify channel (or the fallback
// sleep) spends no model tokens — the zero-token guarantee is preserved.
// lockPaths is one or more pause locks (see internal/lock): every non-empty entry is
// checked FIRST each pass, so an agent notices EITHER its per-project lock OR the
// account-level burn-enforce lock (docs/build-tasks/burn-auto-enforce.md) and can switch
// into `lock wait`. Pass none (or all "") to disable the check.
func Wait(dir string, timeout, interval time.Duration, lockPaths ...string) (WaitResult, error) {
	return WaitAs(Claimant{}, dir, timeout, interval, lockPaths...)
}

// Claimant is the owner a wait is on behalf of. With a By, an in-progress ticket that someone ELSE holds
// a live claim on is that someone's work in hand, not an orphan, and does not wake this owner; its own
// claims, unclaimed tickets and claims that have lapsed still do. The zero value asks nothing of claims:
// every in-progress ticket is an orphan, as it always was for a caller that does not say who it is.
type Claimant struct {
	By string
}

// WaitAs is Wait on behalf of an owner who is identified, so a peer's claimed ticket is left alone.
func WaitAs(who Claimant, dir string, timeout, interval time.Duration, lockPaths ...string) (WaitResult, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return WaitResult{}, fmt.Errorf("role-queue dir not found: %s", dir)
	}
	deadline := time.Now().Add(timeout)

	// check runs the lock→in-progress→pending precedence once; ok reports whether any
	// condition is satisfied. The single source of truth for both the immediate entry
	// check and every re-check, event-driven or fallback.
	skips := newSkipReporter(who)
	defer skips.close()
	skips.watching(dir)
	check := func() (WaitResult, bool) {
		for _, lockPath := range lockPaths {
			if lockPath == "" {
				continue
			}
			if _, err := os.Lstat(lockPath); err == nil {
				return WaitResult{Kind: "LOCKED", Msg: lockPath}, true
			}
		}
		if in := unclaimedByOthers(who, filepath.Join(dir, "in-progress"), actionable(filepath.Join(dir, "in-progress"))); len(in) > 0 {
			return WaitResult{Kind: "ORPHAN-INPROGRESS", Msg: strings.Join(in, " ")}, true
		}
		if p := claimablePending(filepath.Join(dir, "pending")); len(p) > 0 {
			return WaitResult{Kind: "WORK-PENDING", Msg: strings.Join(p, " ")}, true
		}
		// Nothing wakes us: say why any work-looking entry was passed over, so a stranded lane is
		// diagnosable from the wait's log.
		skips.report(dir)
		return WaitResult{}, false
	}

	// Immediate check on entry — no missed-event race, instant return when satisfied.
	if res, ok := check(); ok {
		return res, nil
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		// fsnotify unavailable — degrade to the poll loop (prior behaviour), never break.
		return waitPoll(check, deadline, interval, timeout), nil
	}
	defer w.Close()

	// Watch what exists: pending/ and in-progress/ (may be absent on a fresh role —
	// added on their Create below), the lock file's PARENT dir, and dir itself so a
	// fresh role's subdir creation is seen. Best-effort — a failed Add just means that
	// path leans on the interval safety-net.
	addWatch := func(p string) {
		if _, statErr := os.Stat(p); statErr == nil {
			_ = w.Add(p)
		}
	}
	addWatch(dir)
	addWatch(filepath.Join(dir, "in-progress"))
	addWatch(filepath.Join(dir, "pending"))
	for _, lockPath := range lockPaths {
		if lockPath != "" {
			addWatch(filepath.Dir(lockPath))
		}
	}

	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if time.Until(deadline) <= 0 {
		return WaitResult{Kind: "TIMEOUT", Msg: fmt.Sprintf("re-arm (no work in %s)", timeout)}, nil
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	for {
		// Re-run the FULL check every pass — never trust the triggering event alone
		// (mirrors maybeWake). Also catches the tiny race between the entry check and
		// the watch being armed.
		if res, ok := check(); ok {
			return res, nil
		}
		select {
		case ev, ok := <-w.Events:
			if !ok {
				// Watcher closed unexpectedly — finish out on the poll fallback.
				return waitPoll(check, deadline, interval, timeout), nil
			}
			// A newly created pending/in-progress subdir under a fresh role must join
			// the watch so files landing in it fire events (as addTreeRecursive/
			// handleWatchEvent already do for new dirs).
			if ev.Has(fsnotify.Create) {
				if fi, statErr := os.Stat(ev.Name); statErr == nil && fi.IsDir() {
					_ = w.Add(ev.Name)
				}
			}
		case <-w.Errors:
			// Fall through to the top-of-loop re-check.
		case <-ticker.C:
			// Belt-and-braces re-check (top of loop) — a dropped/coalesced event can't
			// strand the waiter past one interval.
		case <-timer.C:
			return WaitResult{Kind: "TIMEOUT", Msg: fmt.Sprintf("re-arm (no work in %s)", timeout)}, nil
		}
	}
}

// unclaimedByOthers drops from names (entries of ipDir) those another owner holds a live claim on.
func unclaimedByOthers(who Claimant, ipDir string, names []string) []string {
	if who.By == "" {
		return names
	}
	now := time.Now()
	var out []string
	for _, name := range names {
		if _, held := ClaimedByOther(filepath.Join(ipDir, name), who.By, now); held {
			continue
		}
		out = append(out, name)
	}
	return out
}

// waitPoll is Wait's fallback path when fsnotify can't be established (or its watcher
// closes mid-wait): the prior poll+sleep loop, at interval cadence, bounded by deadline.
// Behaviour degrades to today's — never breaks — when the event path is unavailable.
func waitPoll(check func() (WaitResult, bool), deadline time.Time, interval, timeout time.Duration) WaitResult {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if res, ok := check(); ok {
			return res
		}
		if time.Now().After(deadline) || time.Until(deadline) <= 0 {
			return WaitResult{Kind: "TIMEOUT", Msg: fmt.Sprintf("re-arm (no work in %s)", timeout)}
		}
		time.Sleep(interval)
	}
}
