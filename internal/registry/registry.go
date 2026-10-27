// Package registry is leadkit's lead & session identity registry
// (docs/build-tasks/lead-identity-registry.md, registry-local-default.md). It answers "who
// leads what, live or stale". It is LOCAL BY DEFAULT — a plain account dir, no git, no
// setup — because sessions/agents are single-machine; a shared git remote is an OPTIONAL,
// opt-in cross-machine add-on (config registry_remote), NOT a precondition. LOCAL is the
// complete, non-degraded mode: register writes + who reads the local dir with no warning.
// SYNC mode (git commit/pull-rebase/push) engages only when a remote is configured OR the
// dir is already a git repo with an origin (see Store.SyncMode); LOCAL mode never shells git.
//
// THE STORE. A dir at a local path (config registry_repo, default
// ~/.claude/leadkit/registry); in SYNC mode it is a git repo whose origin is a shared
// remote both machines clone. Per-session entry files live under
// sessions/<host>__<session_id>.json — ONE FILE PER SESSION. That is the whole
// conflict-freedom design: two sessions (on the same or different machines) NEVER edit
// the same file, so a `git pull --rebase` across machines merges the disjoint files
// trivially — the only possible conflict is the ref-update race, which the bounded
// pull-rebase-retry in git.go absorbs. The local write mirrors internal/usage/presence.go's
// atomic temp+rename.
//
// OWN-IDENTITY. A session writes ONLY its own entry: host comes from os.Hostname() and
// session_id from the tool ($LEADKIT_SESSION_ID / the project slug), NEVER from a flag —
// the same no-spoofing rule brief's role guards enforce. The file name is derived from
// host+session_id, so a session physically cannot address another session's file.
//
// BEST-EFFORT SYNC. Git/network is NEVER allowed to block or error a session — identity
// is best-effort infra. register writes local first, then best-effort syncs; who pulls
// best-effort then reads, falling back to the local cache marked "(possibly stale)" on
// any git failure. See git.go.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Role is a session's declared role in the registry. A session with role Lead records
// which workstream/project slug(s) it LEADS in Entry.Leads.
type Role string

const (
	RoleLead     Role = "lead"
	RoleIC       Role = "ic"
	RoleReviewer Role = "reviewer"
	RoleWatcher  Role = "watcher"
)

// ValidRoles is the sanctioned role set — `leadkit register --role` refuses anything else.
var ValidRoles = []Role{RoleLead, RoleIC, RoleReviewer, RoleWatcher}

// ValidRole reports whether r is one of the sanctioned roles.
func ValidRole(r string) bool {
	for _, v := range ValidRoles {
		if string(v) == r {
			return true
		}
	}
	return false
}

// Entry is ONE session's registry record — its identity, role, and (for a lead) the
// workstream/project slug(s) it leads. session_id + host are the session's OWN identity
// (resolved by the tool, never a flag); leads is usually [project] but a lead may lead a
// named workstream like "#4217" or "leadkit".
type Entry struct {
	SessionID string   `json:"session_id"`
	Name      string   `json:"name"`
	Host      string   `json:"host"`
	Role      Role     `json:"role"`
	Project   string   `json:"project"`
	Leads     []string `json:"leads,omitempty"`
	// MessagingSocket + HarnessSession are the HARNESS ADDRESSING HANDLE (the same pair
	// usage.PresenceEntry carries): the Claude Code cross-session SendMessage layer
	// addresses by socket/uuid, not by leadkit session_id, so recording them here lets
	// routing.DeliveryHint name a real SendMessage target when it resolves a recipient
	// through the registry (a lead of a workstream whose cwd slug differs). Both are read
	// from env at register time and OMITTED (omitempty) when absent — an older writer's
	// entry simply lacks them and the hint degrades to a session-id handle (backward-safe).
	MessagingSocket string    `json:"messaging_socket,omitempty"`
	HarnessSession  string    `json:"harness_session,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DisplayName is the entry's name, falling back to the session id when unnamed.
func (e Entry) DisplayName() string {
	if e.Name != "" {
		return e.Name
	}
	return e.SessionID
}

// LeadsOrProject is the slug set this entry effectively leads: its explicit Leads, or —
// for a lead that declared none — its own project slug (a lead implicitly leads its
// project). Non-leads return their explicit Leads (usually empty).
func (e Entry) LeadsOrProject() []string {
	if len(e.Leads) > 0 {
		return e.Leads
	}
	if e.Role == RoleLead && e.Project != "" {
		return []string{e.Project}
	}
	return nil
}

// Store is a handle on the registry dir at Repo. The registry is LOCAL by default — a
// plain account dir (like presence/projects), read/written with an atomic temp+rename and
// NO git. Git sync layers on top only in SYNC mode (SyncMode): when a remote is configured
// (remote, from registry_remote) OR the dir is already a git repo with an origin. git.go
// adds the (best-effort) sync + init + prune-sync plumbing the SYNC path uses.
type Store struct {
	Repo   string
	remote string // configured registry_remote (from config), "" = no remote → LOCAL default
	git    GitFunc
}

// Open returns a Store on the registry dir at repo, using the real git binary. No remote is
// configured, so it is LOCAL unless the dir is already a git repo with an origin — chain
// WithRemote to declare a configured registry_remote.
func Open(repo string) *Store { return &Store{Repo: repo, git: defaultGit} }

// OpenWithGit returns a Store whose git calls are served by git — the seam tests use to
// exercise the sync retry loop (and the LOCAL git-never-invoked assertion) without a real
// remote (the disjoint-merge proof still uses real git via Open).
func OpenWithGit(repo string, git GitFunc) *Store { return &Store{Repo: repo, git: git} }

// WithRemote records the configured registry_remote (config) and returns s for chaining. A
// non-empty remote puts the store in SYNC mode (git sync engaged); empty keeps the LOCAL
// default. Callers pass prof.RegistryRemote here.
func (s *Store) WithRemote(remote string) *Store { s.remote = remote; return s }

// SessionsDir is the per-session entry directory under the repo.
func (s *Store) SessionsDir() string { return filepath.Join(s.Repo, "sessions") }

// EntryFileName is the sessions/ file name for a (host, session_id) pair:
// <host>__<session_id>.json, each component sanitized to a safe, git-clean file name.
// The "__" separator is unambiguous because sanitize maps any input "_" to "-".
func EntryFileName(host, sessionID string) string {
	return sanitize(host) + "__" + sanitize(sessionID) + ".json"
}

// relEntryPath is the repo-relative, forward-slash path git operates on.
func relEntryPath(host, sessionID string) string {
	return "sessions/" + EntryFileName(host, sessionID)
}

// sanitize maps s to [A-Za-z0-9.-], turning everything else (including "_", "/", spaces)
// into "-" so the derived file name is a single safe path segment. "" becomes "unknown".
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

// writeLocal atomically writes e to its sessions/ file (temp + rename, mirroring
// usage.WritePresenceEntry) and returns the file name written. An entry with no host or
// session_id has no stable key and is refused (own-identity: an unresolvable identity
// must never be written under a guessed name).
func (s *Store) writeLocal(e Entry) (string, error) {
	if e.Host == "" || e.SessionID == "" {
		return "", fmt.Errorf("registry: entry needs both host and session_id (own-identity)")
	}
	dir := s.SessionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("registry: mkdir sessions dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", fmt.Errorf("registry: marshal entry: %w", err)
	}
	data = append(data, '\n')

	name := EntryFileName(e.Host, e.SessionID)
	path := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, ".leadkit-registry-*.tmp")
	if err != nil {
		return "", fmt.Errorf("registry: create temp entry file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("registry: write temp entry file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("registry: close temp entry file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("registry: rename entry into place: %w", err)
	}
	return name, nil
}

// loadEntry reads the entry stored at file name (if any). ok=false means no readable
// entry there.
func (s *Store) loadEntry(name string) (Entry, bool) {
	data, err := os.ReadFile(filepath.Join(s.SessionsDir(), name))
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if json.Unmarshal(data, &e) != nil {
		return Entry{}, false
	}
	return e, true
}

// ReadAll reads every session entry under the repo. ok=false only on a real read failure;
// a missing sessions/ dir is the ordinary "nothing registered yet" state (ok=true, empty).
// A malformed individual file is skipped (one bad entry never blinds the whole read), the
// same rule usage.ReadPresence follows.
func (s *Store) ReadAll() (entries []Entry, ok bool) {
	dir := s.SessionsDir()
	de, err := os.ReadDir(dir)
	if err != nil {
		return nil, os.IsNotExist(err)
	}
	for _, e := range de {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var entry Entry
		if json.Unmarshal(data, &entry) != nil {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, true
}

// Live returns the subset of entries whose UpdatedAt is at/after cutoff (pass
// now.Add(-ttl)) — a session that stopped touching its entry ages out of the live set.
func Live(entries []Entry, cutoff time.Time) []Entry {
	out := entries[:0:0]
	for _, e := range entries {
		if e.UpdatedAt.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Filter narrows entries by project, role, and host (each empty = no filter). project
// matches an entry whose own project is the slug OR that LEADS the slug (so
// `who --project X` finds both the sessions working X and the lead of X).
type Filter struct {
	Project string
	Role    string
	Host    string
}

// Apply returns the entries matching f, sorted stably (role, then leads, then name).
func (f Filter) Apply(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if f.Role != "" && string(e.Role) != f.Role {
			continue
		}
		if f.Host != "" && e.Host != f.Host {
			continue
		}
		if f.Project != "" && !matchesProject(e, f.Project) {
			continue
		}
		out = append(out, e)
	}
	Sort(out)
	return out
}

func matchesProject(e Entry, slug string) bool {
	if e.Project == slug {
		return true
	}
	for _, l := range e.LeadsOrProject() {
		if l == slug {
			return true
		}
	}
	return false
}

// Sort orders entries stably for display: role, then leads, then name, then host/session.
func Sort(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		la, lb := strings.Join(a.Leads, ","), strings.Join(b.Leads, ",")
		if la != lb {
			return la < lb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.SessionID < b.SessionID
	})
}

// LeadClaim is a workstream/project slug claimed by 2+ DISTINCT live lead sessions —
// the AMBIGUOUS-LEADERSHIP signal `who` flags and doctor warns on (the two-leadkit-lead
// and duplicate-slug cases).
type LeadClaim struct {
	Slug    string
	Leaders []Entry
}

// AmbiguousLeadership finds every slug that 2+ DISTINCT live lead sessions claim to lead
// (a lead with no explicit leads implicitly leads its own project). "Distinct" is by
// host+session_id, so a session appearing once never self-conflicts. Pass the LIVE
// entries (already TTL-filtered).
func AmbiguousLeadership(live []Entry) []LeadClaim {
	bySlug := map[string][]Entry{}
	for _, e := range live {
		if e.Role != RoleLead {
			continue
		}
		for _, slug := range e.LeadsOrProject() {
			bySlug[slug] = append(bySlug[slug], e)
		}
	}
	var out []LeadClaim
	for slug, es := range bySlug {
		if distinctSessions(es) >= 2 {
			Sort(es)
			out = append(out, LeadClaim{Slug: slug, Leaders: es})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func distinctSessions(es []Entry) int {
	seen := map[string]bool{}
	for _, e := range es {
		seen[e.Host+"\x00"+e.SessionID] = true
	}
	return len(seen)
}
