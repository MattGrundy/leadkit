// Package ticketlink ties a ticket to the work that satisfied it, at the two moments a verb already
// holds both facts.
//
// `leadkit pr create --task T` records the PR on the ticket as a full URL in its trackers map
// (RecordPR): `trackers: {pr: https://github.com/o/r/pull/110}`. That survives any merge method and
// names its repository, so whether the work landed is a question for the forge, not for git history.
// `leadkit pr merge` records what the merge became (RecordMerge): sha is the commit main now holds,
// reviewed_sha the head the gate judged, and the merge commit carries the always-on `Leadkit-Merge`
// marker plus `Ticket: T` when a ticket is known (WithTrailers). A merge never writes a status:
// merged and complete are different claims, and closing a ticket stays a deliberate act.
//
// The writes are verbs' writes, not edits: task-status-guard watches the Edit/Write/MultiEdit tools
// and never sees a leadkit process write a file, and nothing here sets a terminal status for it to
// object to.
package ticketlink

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mgpp.dev/leadkit/internal/ticketstore"
)

// TrailerKey is the trailer's key. The value is the ticket id.
const TrailerKey = "Ticket"

// Trailer is the line a merge commit carries for ticket id: `Ticket: 265`.
func Trailer(id string) string { return TrailerKey + ": " + id }

// HasTrailer reports whether message carries the trailer line for id, anywhere in it. It looks at
// every line rather than git's last-paragraph rule, because a forge may append its own trailers
// (Co-authored-by) after ours.
func HasTrailer(message, id string) bool { return hasLine(message, Trailer(id)) }

func hasLine(message, want string) bool {
	for _, line := range strings.Split(message, "\n") {
		if strings.TrimRight(line, " \t\r") == want {
			return true
		}
	}
	return false
}

// MarkerKey and MarkerValue make the marker line every merge made by `leadkit pr merge` carries,
// `Leadkit-Merge: pr-merge`, with or without a ticket. Its absence from a merge commit on the base
// branch is what says the merge was made some other way; the absence of Ticket says only that no
// ticket was linked.
const (
	MarkerKey   = "Leadkit-Merge"
	MarkerValue = "pr-merge"
)

// Marker is the marker line.
func Marker() string { return MarkerKey + ": " + MarkerValue }

// HasMarker reports whether message carries the marker line, anywhere in it.
func HasMarker(message string) bool { return hasLine(message, Marker()) }

// TrailerBlock is the trailer paragraph for a merge: the marker, then the Ticket line when id is
// not empty. The two sit in one paragraph so git reads them as a single trailer block.
func TrailerBlock(id string) string {
	if id == "" {
		return Marker()
	}
	return Marker() + "\n" + Trailer(id)
}

// WithTrailers returns body with TrailerBlock(id) appended as its own final paragraph. Lines the
// body already carries are not repeated.
func WithTrailers(body, id string) string {
	body = strings.TrimRight(body, "\n")
	var add []string
	if !HasMarker(body) {
		add = append(add, Marker())
	}
	if id != "" && !HasTrailer(body, id) {
		add = append(add, Trailer(id))
	}
	if len(add) == 0 {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return strings.Join(add, "\n")
	}
	return body + "\n\n" + strings.Join(add, "\n")
}

// Ref is a ticket resolved to its file in the store.
type Ref struct {
	ID   string // the frontmatter id, digits only
	Path string // the store file, symlinks resolved
}

// Resolve names the ticket task refers to: a bare id (`265`), a name (`265-x` or `265-x.md`) under
// storeRoot, or a path to the file or to a queue entry that links to it. It refuses, before anything
// is merged, a ticket it could not later write: one that is missing, ambiguous, outside the store,
// whose id disagrees with its file name, or whose frontmatter the store writer would reject.
func Resolve(task, storeRoot string) (Ref, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return Ref{}, fmt.Errorf("ticketlink: no ticket given")
	}
	if storeRoot == "" {
		return Ref{}, fmt.Errorf("ticketlink: no store root to look %q up in", task)
	}
	root, err := filepath.EvalSymlinks(storeRoot)
	if err != nil {
		return Ref{}, fmt.Errorf("ticketlink: store root %s: %w", storeRoot, err)
	}

	var candidate string
	switch {
	case allDigits(task):
		matches, _ := filepath.Glob(filepath.Join(root, task+"-*.md"))
		if exact := filepath.Join(root, task+".md"); fileExists(exact) {
			matches = append(matches, exact)
		}
		switch len(matches) {
		case 0:
			return Ref{}, fmt.Errorf("ticketlink: no ticket %s under %s", task, storeRoot)
		case 1:
			candidate = matches[0]
		default:
			return Ref{}, fmt.Errorf("ticketlink: ticket id %s is ambiguous under %s: %s", task, storeRoot, strings.Join(matches, ", "))
		}
	case filepath.IsAbs(task) || strings.ContainsRune(task, filepath.Separator):
		candidate = task
	default:
		candidate = filepath.Join(root, strings.TrimSuffix(task, ".md")+".md")
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return Ref{}, fmt.Errorf("ticketlink: ticket %s not found", task)
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return Ref{}, fmt.Errorf("ticketlink: %s is not a ticket directly under the store %s", real, storeRoot)
	}

	data, err := os.ReadFile(real)
	if err != nil {
		return Ref{}, fmt.Errorf("ticketlink: %w", err)
	}
	doc := ticketstore.Parse(real, string(data))
	if len(doc.Findings) > 0 {
		var parts []string
		for _, f := range doc.Findings {
			parts = append(parts, f.String())
		}
		return Ref{}, fmt.Errorf("ticketlink: %s cannot be written until it is fixed: %s", real, strings.Join(parts, "; "))
	}
	id := doc.Value("id")
	prefix := strings.SplitN(strings.TrimSuffix(filepath.Base(real), ".md"), "-", 2)[0]
	if id == "" || prefix != id {
		return Ref{}, fmt.Errorf("ticketlink: %s has id %q but its file name says %q; refusing to guess which ticket was meant", real, id, prefix)
	}
	return Ref{ID: id, Path: real}, nil
}

// Change is what a record did to a ticket. Status is reported, never written.
type Change struct {
	Status         string
	OldSHA, NewSHA string
	OldPR, NewPR   string
}

// PRTracker is the trackers key a ticket's PR is recorded under.
const PRTracker = "pr"

var prURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+/(?:-/)?(?:pull|merge_requests)/[0-9]+`)

// ExtractPRURL finds the PR or MR URL in the output of the forge CLI that created it, or "". The
// forge CLIs print the new URL last, after any warning or hint that links elsewhere, so the last
// match is the one taken.
func ExtractPRURL(out string) string {
	all := prURLPattern.FindAllString(out, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

// IsPRURL reports whether s is exactly one PR or MR URL.
func IsPRURL(s string) bool { return prURLPattern.FindString(s) == s && s != "" }

// PreflightPR refuses, before anything outward-facing happens, a ticket whose trackers field a PR
// could not be recorded in: one that is not absent, empty or a one-line {k: v} map.
func PreflightPR(ref Ref) error {
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return fmt.Errorf("ticketlink: %w", err)
	}
	doc := ticketstore.Parse(ref.Path, string(data))
	if f, ok := doc.Get("trackers"); ok && (f.Kind != ticketstore.KindMap || f.Style != ticketstore.StyleFlow) {
		return fmt.Errorf("ticketlink: %s: trackers must be an empty or one-line {k: v} map to record a PR in it, and is not", ref.Path)
	}
	return nil
}

// RecordPR records pr, a full PR URL, as trackers.pr on the ticket. A ticket holds ONE PR here, the
// latest: a rework or a split replaces the earlier URL, and the Change says which was replaced.
func RecordPR(ref Ref, pr string) (Change, error) {
	if !IsPRURL(pr) {
		return Change{}, fmt.Errorf("ticketlink: %q is not a PR URL, and a reference that cannot be followed is not evidence", pr)
	}
	var ch Change
	_, err := ticketstore.Update(ref.Path, func(d *ticketstore.Document) error {
		ch = Change{Status: d.Value("status")}
		ch.OldPR, _ = d.Lookup("trackers", PRTracker)
		ch.NewPR = pr
		return d.SetMapEntry("trackers", PRTracker, pr)
	})
	if err != nil {
		return Change{}, err
	}
	return ch, nil
}

// RecordMerge records what a merge of ticket ref became: sha becomes the commit main now holds,
// and reviewedSHA, the head the gate judged, is kept as reviewed_sha because overwriting sha would
// otherwise lose the one commit the gate records are keyed by. When pr is given it is recorded as
// trackers.pr too (the PR that actually merged). Status is left exactly as it was. The write is the
// store's compare-and-swap, so a note appended concurrently is not lost.
func RecordMerge(ref Ref, mergeSHA, reviewedSHA, pr string) (Change, error) {
	if !isFullSHA(mergeSHA) {
		return Change{}, fmt.Errorf("ticketlink: %q is not a full commit sha, and a sha that cannot be looked up is not evidence", mergeSHA)
	}
	if pr != "" && !IsPRURL(pr) {
		return Change{}, fmt.Errorf("ticketlink: %q is not a PR URL", pr)
	}
	var ch Change
	_, err := ticketstore.Update(ref.Path, func(d *ticketstore.Document) error {
		ch = Change{Status: d.Value("status"), OldSHA: d.Value("sha"), NewSHA: mergeSHA}
		ch.OldPR, _ = d.Lookup("trackers", PRTracker)
		if err := d.Set("sha", mergeSHA); err != nil {
			return err
		}
		if isFullSHA(reviewedSHA) {
			if err := d.Set("reviewed_sha", reviewedSHA); err != nil {
				return err
			}
		}
		if pr != "" {
			ch.NewPR = pr
			return d.SetMapEntry("trackers", PRTracker, pr)
		}
		ch.NewPR = ch.OldPR
		return nil
	})
	if err != nil {
		return Change{}, err
	}
	return ch, nil
}

// Claim is a ticket that records a PR. Err is why it could not be resolved for writing when it could
// not be (a parse finding, an id that disagrees with its file name): such a ticket still CLAIMS the
// PR, and is counted, because a claimant that cannot be written is the case the two-claimant check
// exists for, not a reason to skip it.
type Claim struct {
	Ref
	Err error
}

// FindByPR returns every ticket directly under storeRoot that records pr as trackers.pr, resolvable
// or not. It is an exact match on the whole URL, which is what makes finding the ticket at merge a
// lookup rather than a guess. A store with no directory has no tickets and so no claimants; one that
// exists but cannot be read, or a file in it that cannot be read, is an error and never "none".
func FindByPR(storeRoot, pr string) ([]Claim, error) {
	if !IsPRURL(pr) {
		return nil, nil
	}
	if _, err := os.Stat(storeRoot); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ticketlink: store %s: %w", storeRoot, err)
	}
	files, err := filepath.Glob(filepath.Join(storeRoot, "*.md"))
	if err != nil {
		return nil, err
	}
	var out []Claim
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ticketlink: cannot read %s, so cannot tell whether it records %s: %w", f, pr, err)
		}
		if !strings.Contains(string(data), pr) {
			continue // cheap filter before parsing; the exact match is below
		}
		doc := ticketstore.Parse(f, string(data))
		if got, ok := doc.Lookup("trackers", PRTracker); !ok || got != pr {
			continue
		}
		ref, rErr := Resolve(f, storeRoot)
		if rErr != nil {
			ref = Ref{ID: strings.SplitN(strings.TrimSuffix(filepath.Base(f), ".md"), "-", 2)[0], Path: f}
		}
		out = append(out, Claim{Ref: ref, Err: rErr})
	}
	return out, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
