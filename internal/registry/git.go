package registry

// Git-backed sync for the identity registry. REUSES the existing exec.Command("git", …)
// plumbing pattern (internal/team/fetch.go, internal/doctor/git.go) — a plain shell-out,
// no forked git wrapper. Everything here is BEST-EFFORT by contract: a git or network
// failure returns a note, never blocks or errors the session (identity is best-effort
// infra). The per-session-file store means the only cross-machine conflict is the ref
// race on push, which the bounded pull-rebase-retry loop absorbs (disjoint files rebase
// cleanly).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/redact"
)

// GitFunc runs `git -C dir <args…>` (dir "" runs in the process cwd) and returns the
// combined output plus any error. The seam tests inject to drive the sync retry loop
// without a real remote.
type GitFunc func(dir string, args ...string) (out string, err error)

// defaultGit is the real GitFunc: it shells out to the git binary, mirroring
// doctor.runGit / team.defaultGitFetch.
func defaultGit(dir string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		// The arguments can be a remote URL (`remote add origin <url>`, `clone <url>`) and git
		// quotes it again in its own message, so the text of the error is the one place that can
		// carry a credential to the caller's message (the exec error itself is just an exit status). The returned output stays raw: callers parse it.
		shown := make([]string, len(args))
		for i, a := range args {
			shown[i] = redact.Origin(a)
		}
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(shown, " "), err, redact.URLCredentials(strings.TrimSpace(string(out))))
	}
	return string(out), nil
}

// maxSyncRetries bounds the pull-rebase-then-re-push loop on a non-fast-forward push.
// Disjoint per-session files rebase cleanly, so a small cap is enough; beyond it we give
// up best-effort (the local entry stays intact and the next register/who re-syncs).
const maxSyncRetries = 3

// SyncResult reports what a best-effort sync actually managed. It is NEVER an error the
// caller surfaces as a failure — Note carries the human explanation for a degraded path.
type SyncResult struct {
	Attempted bool   // was a git sync attempted at all (repo is a git repo)?
	Committed bool   // did the local commit land?
	Pushed    bool   // did the push to origin succeed?
	Note      string // human note for a degraded/failed path ("" when fully synced)
}

// isGitRepo reports whether Repo is its own git repo (has a .git). Deliberately a direct
// filesystem check rather than `rev-parse --is-inside-work-tree`, so a registry path that
// happens to sit inside an unrelated git checkout is not mistaken for an initialized
// registry.
func (s *Store) isGitRepo() bool {
	_, err := os.Stat(filepath.Join(s.Repo, ".git"))
	return err == nil
}

// IsGitRepo is the exported filesystem-only git-repo check (never shells git) — doctor uses
// it to detect SYNC intent without violating its no-git-shell contract.
func (s *Store) IsGitRepo() bool { return s.isGitRepo() }

// SyncMode reports whether git sync is engaged. It is LOCAL by default: SYNC only when a
// remote is configured (registry_remote) OR the dir is already a git repo with an origin.
// LOCAL mode NEVER shells git — the origin probe runs only after the filesystem isGitRepo
// check passes, so a plain local dir (no .git, no configured remote) short-circuits before
// any git call.
func (s *Store) SyncMode() bool {
	if s.remote != "" {
		return true
	}
	if !s.isGitRepo() {
		return false
	}
	return s.originURL() != ""
}

// syncIfRemote is sync gated by SyncMode: in LOCAL mode the local write is already the
// complete, non-degraded registry, so it returns a zero SyncResult (no git, no note, no
// warning). In SYNC mode it runs the best-effort git sync.
func (s *Store) syncIfRemote(rel, commitMsg string) SyncResult {
	if !s.SyncMode() {
		return SyncResult{}
	}
	return s.sync(rel, commitMsg)
}

// originURL returns the repo's origin remote URL, or "" if there is none / it can't be read.
func (s *Store) originURL() string {
	out, err := s.git(s.Repo, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Initialized reports whether the registry is a git repo with an origin remote — i.e.
// register/who can actually SYNC across machines. When false they still operate on the
// local dir (degrade gracefully), they just can't reach the shared remote.
func (s *Store) Initialized() bool {
	return s.isGitRepo() && s.originURL() != ""
}

// currentBranch resolves the repo's checked-out branch, or "" (detached/unresolvable) —
// callers then fall back to git's own upstream tracking.
func (s *Store) currentBranch() string {
	out, err := s.git(s.Repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	b := strings.TrimSpace(out)
	if b == "" || b == "HEAD" {
		return ""
	}
	return b
}

// InitResult reports what `registry init` did.
type InitResult struct {
	Repo   string
	Remote string
	Action string // cloned | attached | already-initialized | attached-none
	Note   string
}

// Init clones/attaches the shared registry repo to Repo. IDEMPOTENT: an already-git repo
// with an origin is a no-op that reports the existing remote. Otherwise, given a remote,
// it attaches it (existing repo, no origin) or clones it (no repo yet). A missing remote
// with nothing to attach to is an error (there is nothing to sync against).
func (s *Store) Init(remote string) (InitResult, error) {
	if s.isGitRepo() {
		if cur := s.originURL(); cur != "" {
			return InitResult{Repo: s.Repo, Remote: cur, Action: "already-initialized",
				Note: "already a git repo with origin — no-op"}, nil
		}
		if remote == "" {
			return InitResult{Repo: s.Repo, Action: "attached-none",
				Note: "already a git repo but no origin remote; pass --remote (or set registry_remote) to attach one"}, nil
		}
		if _, err := s.git(s.Repo, "remote", "add", "origin", remote); err != nil {
			return InitResult{}, fmt.Errorf("registry init: attach origin: %w", err)
		}
		// Best-effort first sync so the local repo tracks the shared history.
		_, _ = s.git(s.Repo, "fetch", "origin")
		return InitResult{Repo: s.Repo, Remote: remote, Action: "attached"}, nil
	}
	if remote == "" {
		return InitResult{}, fmt.Errorf("registry init: %s is not a git repo and no --remote given (and no registry_remote configured) — nothing to clone", s.Repo)
	}
	if err := os.MkdirAll(filepath.Dir(s.Repo), 0o755); err != nil {
		return InitResult{}, fmt.Errorf("registry init: mkdir parent of %s: %w", s.Repo, err)
	}
	if _, err := s.git("", "clone", remote, s.Repo); err != nil {
		return InitResult{}, fmt.Errorf("registry init: clone %s: %s", redact.Origin(remote), redact.URLCredentials(err.Error()))
	}
	return InitResult{Repo: s.Repo, Remote: remote, Action: "cloned"}, nil
}

// sync is the best-effort git sync shared by register and clear: stage rel, commit,
// then (if there's an origin) pull --rebase + push with bounded retry on a
// non-fast-forward. NEVER returns an error — every failure degrades into a Note.
func (s *Store) sync(rel, commitMsg string) SyncResult {
	if !s.isGitRepo() {
		return SyncResult{Note: "registry is not a git repo — wrote local entry only; run `leadkit registry init --remote <url>` to sync across machines"}
	}
	res := SyncResult{Attempted: true}

	// Stage the one path (git add stages a deletion too, for --clear). A failure here is
	// best-effort: an untracked, already-removed file (--clear that never synced) has
	// nothing to stage — not an error worth surfacing.
	if _, err := s.git(s.Repo, "add", rel); err != nil {
		res.Note = "nothing to stage (best-effort): " + trimGitErr(err)
		return res
	}
	if out, err := s.git(s.Repo, "commit", "-m", commitMsg); err != nil {
		if strings.Contains(out, "nothing to commit") {
			// Idempotent --touch with no content change: nothing to sync, not a failure.
			res.Committed = true
		} else {
			res.Note = "git commit failed (best-effort): " + trimGitErr(err)
			return res
		}
	} else {
		res.Committed = true
	}

	if s.originURL() == "" {
		res.Note = "no origin remote — committed locally only; run `leadkit registry init --remote <url>` to sync across machines"
		return res
	}

	branch := s.currentBranch()
	for attempt := 0; attempt < maxSyncRetries; attempt++ {
		// Pull-rebase first so a concurrent push from another machine is absorbed by
		// rebasing our disjoint file on top of it; best-effort (a failing pull just means
		// the push below will fail non-fast-forward and we retry).
		_, _ = s.pullRebase(branch)
		if _, err := s.push(branch); err == nil {
			res.Pushed = true
			return res
		}
	}
	res.Note = "push failed after retries (non-fast-forward or offline) — local entry intact, next register/who will re-sync"
	return res
}

// PullBestEffort is who's read-path sync: pull --rebase (falling back to the upstream
// when the branch is unresolvable). ok=false on any git failure — the caller then reads
// the local cache and marks the output possibly-stale. NEVER blocks or errors.
func (s *Store) PullBestEffort() (ok bool, note string) {
	if !s.SyncMode() {
		// LOCAL mode: the local dir IS the registry — nothing to sync, nothing stale. This
		// is the complete, non-degraded read (no "possibly stale"/"not initialized" framing).
		return true, ""
	}
	if !s.isGitRepo() {
		return false, "registry not initialized (not a git repo) — showing local entries only; run `leadkit registry init --remote <url>`"
	}
	if s.originURL() == "" {
		return false, "registry has no origin remote — showing local entries only; run `leadkit registry init --remote <url>`"
	}
	if _, err := s.pullRebase(s.currentBranch()); err != nil {
		return false, "sync failed (" + trimGitErr(err) + ") — showing the local cache"
	}
	return true, ""
}

func (s *Store) pullRebase(branch string) (string, error) {
	if branch != "" {
		return s.git(s.Repo, "pull", "--rebase", "origin", branch)
	}
	return s.git(s.Repo, "pull", "--rebase")
}

func (s *Store) push(branch string) (string, error) {
	if branch != "" {
		return s.git(s.Repo, "push", "origin", branch)
	}
	return s.git(s.Repo, "push")
}

// RegisterParams is one session declaring ITS OWN registry entry. Host and SessionID are
// the session's own identity (resolved by the tool, never a flag). Touch refreshes only
// updated_at (heartbeat); Clear removes this session's entry (graceful exit).
type RegisterParams struct {
	SessionID string
	Name      string
	Host      string
	Role      Role
	Project   string
	Leads     []string
	// MessagingSocket + HarnessSession are this session's HARNESS ADDRESSING HANDLE (from
	// env, best-effort), persisted so DeliveryHint can name a real SendMessage target when
	// it resolves a recipient through the registry. Empty is fine (backward-safe).
	MessagingSocket string
	HarnessSession  string
	Now             time.Time
	Touch           bool
	Clear           bool
}

// Register writes/updates/removes THIS session's entry and best-effort syncs. It only
// ever touches the file keyed by (Host, SessionID) — a session cannot write another's
// entry. Returns the resulting entry (zero for --clear) and the sync outcome.
func (s *Store) Register(p RegisterParams) (Entry, SyncResult, error) {
	rel := relEntryPath(p.Host, p.SessionID)
	name := EntryFileName(p.Host, p.SessionID)

	if p.Clear {
		_ = os.Remove(filepath.Join(s.SessionsDir(), name))
		return Entry{}, s.syncIfRemote(rel, fmt.Sprintf("unregister %s@%s", displayFor(p.Name, p.SessionID), p.Host)), nil
	}

	prev, hadPrev := s.loadEntry(name)
	e := Entry{
		SessionID:       p.SessionID,
		Name:            p.Name,
		Host:            p.Host,
		Role:            p.Role,
		Project:         p.Project,
		Leads:           p.Leads,
		MessagingSocket: p.MessagingSocket,
		HarnessSession:  p.HarnessSession,
		StartedAt:       p.Now,
		UpdatedAt:       p.Now,
	}
	if hadPrev {
		e.StartedAt = prev.StartedAt // started_at is set once, at first register
		if e.Name == "" {
			e.Name = prev.Name
		}
		// The harness handle persists across re-registers/heartbeats: a caller that no
		// longer has the env set (or a --touch heartbeat) keeps the last known handle
		// rather than blanking a still-valid SendMessage target.
		if e.MessagingSocket == "" {
			e.MessagingSocket = prev.MessagingSocket
		}
		if e.HarnessSession == "" {
			e.HarnessSession = prev.HarnessSession
		}
		if p.Touch {
			// Heartbeat: preserve the declared identity, refresh only updated_at.
			e.Role = prev.Role
			e.Leads = prev.Leads
			if e.Project == "" {
				e.Project = prev.Project
			}
		}
	}

	if _, err := s.writeLocal(e); err != nil {
		return e, SyncResult{}, err
	}
	return e, s.syncIfRemote(rel, fmt.Sprintf("register %s@%s", e.DisplayName(), e.Host)), nil
}

func displayFor(name, sessionID string) string {
	if name != "" {
		return name
	}
	return sessionID
}

// PruneResult reports the stale-entry prune (dry-run by default).
type PruneResult struct {
	Candidates []Entry // stale entries (UpdatedAt before cutoff)
	Removed    []Entry // actually removed (apply only)
	Sync       SyncResult
}

// Prune finds every entry staler than cutoff (pass now.Add(-ttl)); with apply it removes
// their files and best-effort syncs the removal. Dry-run (apply=false) mutates nothing.
func (s *Store) Prune(cutoff time.Time, apply bool) (PruneResult, error) {
	entries, ok := s.ReadAll()
	if !ok {
		return PruneResult{}, fmt.Errorf("registry prune: could not read %s", s.SessionsDir())
	}
	var res PruneResult
	for _, e := range entries {
		if e.UpdatedAt.Before(cutoff) {
			res.Candidates = append(res.Candidates, e)
		}
	}
	Sort(res.Candidates)
	if !apply || len(res.Candidates) == 0 {
		return res, nil
	}
	for _, e := range res.Candidates {
		name := EntryFileName(e.Host, e.SessionID)
		if err := os.Remove(filepath.Join(s.SessionsDir(), name)); err == nil || os.IsNotExist(err) {
			res.Removed = append(res.Removed, e)
		}
	}
	// Stage the whole sessions/ dir so all deletions are captured in one commit (SYNC mode
	// only — LOCAL prune just removes the files, no git).
	res.Sync = s.syncIfRemote("sessions", fmt.Sprintf("prune %d stale registry entr%s", len(res.Removed), plural(len(res.Removed))))
	return res, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// trimGitErr shortens a git error for a one-line note.
func trimGitErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}
