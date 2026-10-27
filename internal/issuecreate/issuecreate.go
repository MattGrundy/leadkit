// Package issuecreate is `leadkit issue create` — the schema-enforced Issue-creation
// primitive (taxonomy: Project -> Milestone -> Epic -> Issue -> Task,
// docs/HIERARCHY-AND-SYNC.md). An Issue's id-space is DELIBERATELY distinct from a
// Task's (an "I" prefix — I1, I2, ...) so a Task's `parent:` value unambiguously
// names an Issue and never collides with a bare-integer Task id.
//
// NOT confirm-guarded — creating one issue is a normal workflow op (like `task
// create`, `queue move`/`queue assign`), not a bulk mutation. But every test and
// dogfood run MUST target a temp store — see cmd/leadkit's sandboxed runCLI test
// helper; a stray real invocation adds junk to the live queue/store, the same
// incident class task create's own doc comment warns about.
//
// Ad-hoc creation of an issues/ file by hand (bypassing this) is deprecated — a
// later guard can deny raw creation outside leadkit, mirroring task create's own
// note.
package issuecreate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/hierarchy"
)

// Options is one `issue create` invocation's inputs.
type Options struct {
	Title    string
	Epic     string // required — must resolve to a real epic id in milestones.yaml
	Priority string // optional, written as-is
	// Status overrides the default "pending" — `hierarchy apply` uses this to
	// write its derived status (all-terminal Tasks -> "completed"); a bare `issue
	// create` invocation leaves this empty.
	Status string

	IssuesRoot string // issues/ — where the new file is written
	StoreRoot  string // tasks/ — where milestones.yaml lives, for --epic validation
}

// Result is what Create actually did.
type Result struct {
	ID   string // e.g. "I42" — the Issue's own id-space, distinct from a Task id
	Path string
}

// nowFunc is the clock Create's created/updated fields use — injectable so tests can
// assert an exact date (mirrors internal/taskcreate's own nowFunc).
var nowFunc = time.Now

// idPrefix distinguishes an Issue id from a Task id (a bare integer) and an Epic id
// (a kebab-case string) at a glance, and — the real reason — so a Task's `parent:`
// value is NEVER ambiguous between the two id-spaces.
const idPrefix = "I"

// Create validates opts, atomically claims the next Issue id, and writes the new
// issue's canonical frontmatter into IssuesRoot.
func Create(opts Options) (Result, error) {
	if strings.TrimSpace(opts.Title) == "" {
		return Result{}, fmt.Errorf("issuecreate: --title is required")
	}
	if strings.TrimSpace(opts.Epic) == "" {
		return Result{}, fmt.Errorf("issuecreate: --epic is required")
	}

	model, _, err := hierarchy.Parse(filepath.Join(opts.StoreRoot, "milestones.yaml"))
	if err != nil {
		return Result{}, fmt.Errorf("issuecreate: parse milestones.yaml: %w", err)
	}
	if !model.EpicIDs()[opts.Epic] {
		return Result{}, fmt.Errorf("issuecreate: --epic %q does not resolve to an existing epic in milestones.yaml", opts.Epic)
	}

	if err := os.MkdirAll(opts.IssuesRoot, 0o755); err != nil {
		return Result{}, fmt.Errorf("issuecreate: create issues root %s: %w", opts.IssuesRoot, err)
	}

	slug := slugify(opts.Title)
	id, path, err := claimNextID(opts.IssuesRoot, slug)
	if err != nil {
		return Result{}, err
	}

	content := buildFrontmatter(id, opts)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Result{}, fmt.Errorf("issuecreate: write %s: %w", path, err)
	}
	return Result{ID: id, Path: path}, nil
}

// buildFrontmatter matches taskcreate's own canonical shape as closely as an Issue's
// fields allow: id, title, status(default pending), parent(epic), trackers:{},
// created, updated.
func buildFrontmatter(id string, opts Options) string {
	now := nowFunc().UTC().Format("2006-01-02")
	status := opts.Status
	if status == "" {
		status = "pending"
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", id)
	fmt.Fprintf(&b, "title: %s\n", opts.Title)
	fmt.Fprintf(&b, "status: %s\n", status)
	fmt.Fprintf(&b, "parent: %s\n", opts.Epic)
	if opts.Priority != "" {
		fmt.Fprintf(&b, "priority: %s\n", opts.Priority)
	}
	fmt.Fprintf(&b, "created: %s\n", now)
	fmt.Fprintf(&b, "updated: %s\n", now)
	b.WriteString("trackers: {}\n")
	b.WriteString("---\n\n")
	return b.String()
}

// slugify mirrors internal/taskcreate's own (duplicated on purpose — this
// codebase's small-package decoupling convention).
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
		s = "issue"
	}
	return s
}

// maxClaimAttempts bounds the retry loop, mirroring taskcreate's own.
const maxClaimAttempts = 1000

// claimNextID mirrors taskcreate's claimNextID exactly (same atomic-marker-file
// doctrine — see that package's own doc comment for the full race-condition
// rationale) but in the Issue id-space ("I<N>" instead of a bare integer).
func claimNextID(issuesRoot, slug string) (id string, path string, err error) {
	start := scanMaxID(issuesRoot) + 1
	for n := start; n < start+maxClaimAttempts; n++ {
		id = idPrefix + strconv.Itoa(n)
		claimPath := filepath.Join(issuesRoot, ".claim-"+id)
		f, cerr := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if cerr != nil {
			if os.IsExist(cerr) {
				continue // another creator already claimed this id
			}
			return "", "", fmt.Errorf("issuecreate: claim id %s: %w", id, cerr)
		}
		f.Close()

		if idAlreadyTaken(issuesRoot, id) {
			os.Remove(claimPath)
			continue
		}

		finalPath := filepath.Join(issuesRoot, fmt.Sprintf("%s-%s.md", id, slug))
		if err := os.Rename(claimPath, finalPath); err != nil {
			return "", "", fmt.Errorf("issuecreate: finalize id %s: %w", id, err)
		}
		return id, finalPath, nil
	}
	return "", "", fmt.Errorf("issuecreate: could not claim an id after %d attempts", maxClaimAttempts)
}

// NextIDNumber returns the bare number claimNextID's NEXT call would assign
// (scanMaxID+1), WITHOUT claiming anything — for a caller that needs to preview a
// sequence of ids it would create (e.g. `hierarchy apply`'s dry-run, previewing
// several new issues in one plan) without touching disk. The real claim can still
// drift if something else writes concurrently between preview and creation — the
// same caveat every sequential-id scheme in this codebase already accepts.
func NextIDNumber(issuesRoot string) int {
	return scanMaxID(issuesRoot) + 1
}

// scanMaxID returns the highest numeric suffix among existing "I<N>-*.md" files in
// issuesRoot, or 0 if none. Marker files (".claim-I<N>") never count.
func scanMaxID(issuesRoot string) int {
	entries, err := os.ReadDir(issuesRoot)
	if err != nil {
		return 0
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasPrefix(name, idPrefix) {
			continue
		}
		rest := name[len(idPrefix):]
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(rest) || rest[i] != '-' {
			continue
		}
		n, convErr := strconv.Atoi(rest[:i])
		if convErr != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max
}

// idAlreadyTaken is the defensive check behind the claim-marker's own O_EXCL.
func idAlreadyTaken(issuesRoot, id string) bool {
	matches, _ := filepath.Glob(filepath.Join(issuesRoot, id+"-*.md"))
	return len(matches) > 0
}
