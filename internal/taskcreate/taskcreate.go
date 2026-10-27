// Package taskcreate is `leadkit task create` — the single sanctioned
// task-creation primitive. New tasks going through this (instead of being
// hand-written) is the fix at the SOURCE for the noise internal/backfill and
// internal/audit's other passes clean up after the fact: owner=null, branch-name
// owners, missing type, id-collision races, and (as of the taxonomy lock) an
// unparented task needing a later mapping backfill at all. Schema-enforced: every
// task is now born typed (--type) AND parented to a real Issue (--issue) — see
// Create's own validation — so those backfills become one-time, not recurring.
//
// NOT confirm-guarded — creating one task is a normal workflow op (like `queue
// move`/`queue assign`), not a bulk mutation. But every test and dogfood run MUST
// target a temp store — see cmd/leadkit's sandboxed runCLI test helper and this
// task's own safety note: a stray real invocation adds junk to the live queue, the
// same incident class as the audit --apply mistake.
package taskcreate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/hierarchy"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/typeinfer"
)

// Options is one `task create` invocation's inputs.
type Options struct {
	Title string
	Type  string // must be canonical (typeinfer.IsCanonical against AgentsDir's set)
	// Parent is this task's Issue id (taxonomy: Epic -> Issue -> Task) — validated
	// against IssuesRoot unless Unparented is set. Written as `parent:` verbatim.
	Parent     string
	Unparented bool   // explicit escape hatch: allows Parent == "" (edge cases only)
	Milestone  string // optional; validated against milestones.yaml only if it parses AND has milestones (absence = unvalidated, hierarchy not built yet)
	Priority   string // optional, written as-is
	Owner      string // optional, written as-is
	Assign     string // optional role name — symlinks the new task via queue.Assign
	// RosterTypes are the leadkit.yaml roster role names, unioned into queue.Assign's
	// known-role set (with FixedRoles + the legacy ROLES file) so --assign resolves a
	// role declared in the roster but not the ROLES file. Nil is fine (roster-less).
	RosterTypes []string

	StoreRoot  string
	QueueRoot  string
	RolesPath  string
	AgentsDir  string // team/agents/ — the canonical type set source
	IssuesRoot string // issues/ — where --issue is validated against

	// BriefID, when set, is written as a `brief_id:` frontmatter key — the back-link a
	// routing brief materialized via `leadkit brief ack --as-task` carries to the brief
	// it came from. Empty for an ordinary task. Generic here on purpose: taskcreate does
	// not import routing, it just persists the reference string it is handed.
	BriefID string
	// Body, when non-empty, is written as the task's markdown body (after the closing
	// frontmatter fence) — e.g. a materialized brief's subject + a pointer back to it.
	Body string
}

// Result is what Create actually did.
type Result struct {
	ID   int
	Path string
	Lane string // "" unless Assign was set and succeeded
}

// nowFunc is the clock Create's created/updated fields use — injectable so tests can
// assert an exact date.
var nowFunc = time.Now

// Create validates opts, atomically claims the next store id, writes the new task's
// canonical frontmatter, and — if Assign is set — symlinks it into that role's
// pending/ via queue.Assign (reusing its own dedup/refusal checks).
func Create(opts Options) (Result, error) {
	if strings.TrimSpace(opts.Title) == "" {
		return Result{}, fmt.Errorf("taskcreate: --title is required")
	}

	// Validation path: fall back to the embedded set so a clone with no captured snapshot
	// rejects a genuinely wrong type rather than every type. See LoadCanonicalOrEmbedded.
	canonical := typeinfer.LoadCanonicalOrEmbedded(opts.AgentsDir)
	typ, ok := typeinfer.IsCanonical(opts.Type, canonical)
	if !ok {
		// Name the real source: when the project's agents dir is absent or empty the types
		// came from the binary's embedded set, and pointing at an empty directory sends the
		// reader to look for a file that was never consulted.
		src := opts.AgentsDir
		if len(typeinfer.LoadCanonical(opts.AgentsDir)) == 0 {
			src = "the binary's embedded starter set (" + opts.AgentsDir + " is absent or empty)"
		}
		known := make([]string, 0, len(canonical))
		for k := range canonical {
			known = append(known, k)
		}
		sort.Strings(known)
		return Result{}, fmt.Errorf("taskcreate: --type %q is not canonical (known types come from %s): %s", opts.Type, src, strings.Join(known, ", "))
	}

	if strings.TrimSpace(opts.Parent) == "" && !opts.Unparented {
		return Result{}, fmt.Errorf("taskcreate: --issue is required (every task is born parented to an Issue — pass --unparented for an explicit orphan edge case)")
	}
	if opts.Parent != "" {
		issues, err := hierarchy.LoadIssues(opts.IssuesRoot)
		if err != nil {
			return Result{}, fmt.Errorf("taskcreate: load issues: %w", err)
		}
		if !issueExists(issues, opts.Parent) {
			return Result{}, fmt.Errorf("taskcreate: --issue %s does not resolve to an existing issue in %s", opts.Parent, opts.IssuesRoot)
		}
	}
	if opts.Milestone != "" && milestoneKnownInvalid(opts.StoreRoot, opts.Milestone) {
		return Result{}, fmt.Errorf("taskcreate: --milestone %s does not resolve in milestones.yaml", opts.Milestone)
	}

	slug := slugify(opts.Title)
	id, path, err := claimNextID(opts.StoreRoot, slug)
	if err != nil {
		return Result{}, err
	}

	content := buildFrontmatter(id, typ, opts)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Result{}, fmt.Errorf("taskcreate: write %s: %w", path, err)
	}

	res := Result{ID: id, Path: path}
	if opts.Assign != "" {
		assignRes, err := queue.Assign(opts.QueueRoot, opts.StoreRoot, opts.RolesPath, opts.RosterTypes, strconv.Itoa(id), opts.Assign, false)
		if err != nil {
			return res, fmt.Errorf("taskcreate: created %d but --assign failed: %w", id, err)
		}
		res.Lane = assignRes.Role
	}
	return res, nil
}

// buildFrontmatter matches docs/HIERARCHY-AND-SYNC.md's canonical shape exactly
// (its "Task creation goes through leadkit" section: "id, title, status, type,
// parent (+ optional priority/deps)"): id, title, status, type, [parent],
// [milestone], [priority], [owner], created, updated, trackers:{} (empty — a later
// sync step fills it in). NOTE: this task's own build-task doc said "subject(=title)"
// — the design doc (dated the same day, and matching what real recent store tasks
// already use) is authoritative here, so the field written is `title`, not `subject`.
func buildFrontmatter(id int, typ string, opts Options) string {
	now := nowFunc().UTC().Format("2006-01-02")
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %d\n", id)
	fmt.Fprintf(&b, "title: %s\n", opts.Title)
	b.WriteString("status: pending\n")
	fmt.Fprintf(&b, "type: %s\n", typ)
	if opts.Parent != "" {
		fmt.Fprintf(&b, "parent: %s\n", opts.Parent)
	}
	if opts.Milestone != "" {
		fmt.Fprintf(&b, "milestone: %s\n", opts.Milestone)
	}
	if opts.Priority != "" {
		fmt.Fprintf(&b, "priority: %s\n", opts.Priority)
	}
	if opts.Owner != "" {
		fmt.Fprintf(&b, "owner: %s\n", opts.Owner)
	}
	if opts.BriefID != "" {
		fmt.Fprintf(&b, "brief_id: %s\n", opts.BriefID)
	}
	fmt.Fprintf(&b, "created: %s\n", now)
	fmt.Fprintf(&b, "updated: %s\n", now)
	b.WriteString("trackers: {}\n")
	b.WriteString("---\n\n")
	if strings.TrimSpace(opts.Body) != "" {
		b.WriteString(strings.TrimRight(opts.Body, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// slugify turns title into a filename-safe slug: lowercase, non-alphanumeric runs
// collapsed to a single "-", trimmed, capped at a sane length, "task" if empty.
func slugify(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// maxClaimAttempts bounds the retry loop — a real collision storm this deep would
// mean something else is badly wrong; better to error out than spin forever.
const maxClaimAttempts = 1000

// claimNextID is the doctrine's fix for the "read max, compute +1, write later"
// race: it claims the ID NUMBER itself, atomically, via O_EXCL on a small marker
// file (".claim-<id>") — NOT by O_EXCL'ing the final "<id>-<slug>.md" path directly,
// because two concurrent creators with DIFFERENT titles would get different slugs
// and never collide on that check even though they picked the same id. Only the
// single process that wins the marker's O_EXCL rename it into the real final path,
// so the marker is never left behind on the happy path. On any collision (the
// marker already exists, or — belt and braces — a matching task file somehow
// already exists without a marker), it retries with the next id. A crash between
// claim and rename leaves an orphaned ".claim-N" marker, which permanently retires
// id N rather than risk collision or reuse — an acceptable, INTENTIONAL cost (never
// reuse an id).
func claimNextID(storeRoot, slug string) (id int, path string, err error) {
	start := scanMaxID(storeRoot) + 1
	for id = start; id < start+maxClaimAttempts; id++ {
		claimPath := filepath.Join(storeRoot, fmt.Sprintf(".claim-%d", id))
		f, cerr := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if cerr != nil {
			if os.IsExist(cerr) {
				continue // another creator already claimed this id
			}
			return 0, "", fmt.Errorf("taskcreate: claim id %d: %w", id, cerr)
		}
		f.Close()

		if idAlreadyTaken(storeRoot, id) {
			os.Remove(claimPath)
			continue
		}

		finalPath := filepath.Join(storeRoot, fmt.Sprintf("%d-%s.md", id, slug))
		if err := os.Rename(claimPath, finalPath); err != nil {
			return 0, "", fmt.Errorf("taskcreate: finalize id %d: %w", id, err)
		}
		return id, finalPath, nil
	}
	return 0, "", fmt.Errorf("taskcreate: could not claim an id after %d attempts", maxClaimAttempts)
}

// scanMaxID returns the highest "<N>-*" / "<N>.md" numeric id present in storeRoot,
// or 0 if none. Marker files (".claim-N") never count — they aren't tasks.
func scanMaxID(storeRoot string) int {
	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		return 0
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		i := 0
		for i < len(name) && name[i] >= '0' && name[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(name) || (name[i] != '-' && name[i] != '.') {
			continue
		}
		n, convErr := strconv.Atoi(name[:i])
		if convErr != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max
}

// idAlreadyTaken is the defensive check behind the claim-marker's own O_EXCL — a
// real task file for id already existing despite no marker (e.g. from before this
// tool existed) must still block reuse.
func idAlreadyTaken(storeRoot string, id int) bool {
	matches, _ := filepath.Glob(filepath.Join(storeRoot, fmt.Sprintf("%d-*.md", id)))
	if len(matches) > 0 {
		return true
	}
	_, err := os.Stat(filepath.Join(storeRoot, fmt.Sprintf("%d.md", id)))
	return err == nil
}

// issueExists reports whether id names one of issues — `task create`'s --issue
// validation. Its own package doc note applies here too: LoadIssues(opts.IssuesRoot)
// returning empty (issues/ not seeded yet) means this always refuses, correctly —
// --issue is required, so an empty issues/ means nothing can be created until
// `issue create` seeds at least one.
func issueExists(issues []hierarchy.Issue, id string) bool {
	for _, is := range issues {
		if is.ID == id {
			return true
		}
	}
	return false
}

func milestonesPath(storeRoot string) string {
	return filepath.Join(storeRoot, "milestones.yaml")
}

// milestoneKnownInvalid reports whether id is DEFINITELY not a real milestone —
// true only when milestones.yaml parses AND has at least one milestone listed AND
// id isn't among them. Any other case (file absent, parse error, or an empty
// milestones: list) means "can't validate yet" — --milestone stays accepted
// unvalidated, matching this flag's existing "hierarchy not built yet" degrade
// behavior rather than refusing on an ambiguous state.
func milestoneKnownInvalid(storeRoot, id string) bool {
	model, found, err := hierarchy.Parse(milestonesPath(storeRoot))
	if err != nil || !found || len(model.Milestones) == 0 {
		return false
	}
	return !model.MilestoneIDs()[id]
}
