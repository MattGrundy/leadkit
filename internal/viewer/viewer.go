// Package viewer serves a local, read-only web view over the on-disk task queue.
//
// The queue is laid out as <root>/<lane>/<status>/<task>.md, where the *directory*
// is the source of truth for a task's status (directory-as-state). This package
// never writes to the queue — it only reads and renders it, so it is safe to run
// against a live queue without impacting any session.
package viewer

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/audit"
	"mgpp.dev/leadkit/internal/roles"
)

// legacyLaneGroup labels a lane chip whose lane dir name resolves to no
// canonical role at all — role-keyed-lanes.md's "non-role / legacy" group for a
// mixed queue's un-mappable ephemeral/per-owner-instance lanes.
const legacyLaneGroup = "non-role / legacy"

// laneChipLabel annotates lane (a physical queue lane dir name) with the
// canonical ROLE it reads as, via roles.CanonicalLane — the SAME name→role
// resolver send-to-review/gate-verdict/assign/doctor's queue-lanes check all
// route through. A lane that's already a proper role lane (resolves to itself)
// is shown plain; a legacy/instance lane shows "<lane> → <role>"; an
// un-mappable lane is tagged "(non-role / legacy)". This is display-only — the
// underlying filter/Active matching in laneChip still keys on the raw lane name,
// so every lane stays individually filterable regardless of how it's labelled.
func laneChipLabel(lane string, known []string) string {
	role, ok := roles.CanonicalLane(lane, known, nil)
	if !ok {
		return lane + " (" + legacyLaneGroup + ")"
	}
	if role == lane {
		return lane
	}
	return lane + " → " + role
}

// ownerRoleBucket folds a raw owner value to the canonical ROLE chip it belongs
// under, via the SAME roles.CanonicalLane resolver laneChipLabel uses — the OWNER
// facet's bucket key (board-role-facets-hide-empty.md, Change 1). Every owner
// instance that folds to a role (ci-infra, ci-infra-agent, owner-ci-infra, …)
// shares that role's single chip instead of minting one chip per raw instance;
// an owner mapping to NO known role collapses into the ONE legacyLaneGroup
// bucket. "" (no owner) yields "" — no chip. This is render-only: it keys the
// chip + the folded owner filter match, it never rewrites any owner on disk.
func ownerRoleBucket(owner string, known []string) string {
	if owner == "" {
		return ""
	}
	if role, ok := roles.CanonicalLane(owner, known, nil); ok {
		return role
	}
	return legacyLaneGroup
}

// knownStatuses are the columns shown, in order. Any other leaf dir is grouped
// under "other" so nothing is silently hidden.
var knownStatuses = []string{"pending", "in-progress", "blocked", "needs-rework", "done"}

func isStatus(name string) bool {
	for _, s := range knownStatuses {
		if s == name {
			return true
		}
	}
	return false
}

// Task is one queue entry, derived from its path + frontmatter. Read-only.
type Task struct {
	Rel      string            // path relative to its base (stable id)
	Src      string            // "" = queue overlay, "store" = canonical tasks/ store
	Lane     string            // owning lane/agent dir (queue) or owner (store)
	Status   string            // from the status directory (queue) or frontmatter (store)
	Title    string            // frontmatter title/subject, first heading, or filename
	Owner    string            // frontmatter owner, if any
	Meta     map[string]string // parsed frontmatter
	Body     string            // markdown body (post-frontmatter)
	MetaKeys []string          // stable order for rendering Meta
}

// doneStatuses collapse in the board (queue "done" + store frontmatter "completed").
func isDone(status string) bool { return status == "done" || status == "completed" }

// Scan walks the queue root and returns every task file living under a status dir.
// Files not under a recognised status dir (top-level docs, etc.) are ignored.
func Scan(root string) ([]Task, error) {
	var tasks []Task
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than abort the whole scan
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".md" && ext != ".json" {
			return nil
		}
		statusDir := filepath.Base(filepath.Dir(p))
		lane := filepath.Base(filepath.Dir(filepath.Dir(p)))
		status := statusDir
		if !isStatus(statusDir) {
			status = "other"
		}
		rel, _ := filepath.Rel(root, p)
		t := Task{Rel: rel, Lane: lane, Status: status}
		if b, e := os.ReadFile(p); e == nil {
			meta, keys, body := parseFrontmatter(string(b))
			t.Meta, t.MetaKeys, t.Body = meta, keys, body
			t.Owner = meta["owner"]
			t.Title = deriveTitle(meta, body, p)
		} else {
			t.Title = filepath.Base(p)
		}
		tasks = append(tasks, t)
		return nil
	})
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Lane != tasks[j].Lane {
			return tasks[i].Lane < tasks[j].Lane
		}
		return tasks[i].Title < tasks[j].Title
	})
	return tasks, err
}

// ScanStore reads every task in the canonical flat store, grouping by the
// frontmatter `status` field (the store is the source of truth for the full task
// history; the queue is only a symlink overlay of currently-placed tasks).
func ScanStore(store string) ([]Task, error) {
	var tasks []Task
	err := filepath.WalkDir(store, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Ext(p)) != ".md" {
			return nil
		}
		rel, _ := filepath.Rel(store, p)
		t := Task{Rel: rel, Src: "store"}
		if b, e := os.ReadFile(p); e == nil {
			meta, keys, body := parseFrontmatter(string(b))
			t.Meta, t.MetaKeys, t.Body = meta, keys, body
			t.Owner = meta["owner"]
			t.Title = deriveTitle(meta, body, p)
			t.Status = strings.TrimSpace(meta["status"])
			t.Lane = meta["owner"]
		}
		if t.Status == "" {
			t.Status = "unknown"
		}
		if t.Title == "" {
			t.Title = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		}
		if t.Lane == "" {
			t.Lane = "—"
		}
		tasks = append(tasks, t)
		return nil
	})
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Title < tasks[j].Title })
	return tasks, err
}

func deriveTitle(meta map[string]string, body, path string) string {
	if t := strings.TrimSpace(meta["title"]); t != "" {
		return t
	}
	if t := strings.TrimSpace(meta["subject"]); t != "" {
		return t
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// parseFrontmatter reads a leading --- ... --- block as key: value pairs.
// Deliberately minimal (stdlib-only, no YAML dep) — handles the flat key: value
// frontmatter these task files use; anything richer just falls into the body.
func parseFrontmatter(s string) (map[string]string, []string, string) {
	meta := map[string]string{}
	var keys []string
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return meta, keys, s
	}
	i := 1
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			i++
			break
		}
		line := lines[i]
		if idx := strings.Index(line, ":"); idx > 0 {
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+1:])
			v = strings.Trim(v, `"'`)
			if k != "" {
				if _, seen := meta[k]; !seen {
					keys = append(keys, k)
				}
				meta[k] = v
			}
		}
	}
	return meta, keys, strings.Join(lines[i:], "\n")
}

// Server is the read-only HTTP handler over a queue root + optional canonical store.
type Server struct {
	Root  string // queue overlay root (<lane>/<status>/ symlinks)
	Store string // canonical flat tasks/ store (optional; "" disables /store)

	// The rest are /health's audit inputs (see internal/audit) — all optional. An
	// empty SessionTasksRoot just means the session lens contributes nothing; /health
	// always renders, never 500s, regardless of what's missing.
	SessionTasksRoot  string
	CanonicalStatuses []string
	WorkishThreshold  int
	WorkishMarkers    []string

	// PushViolationsLog is push-guard's JSONL deny log (internal/hook.PushGuard) —
	// optional; "" or an absent/unreadable file just means /health's push-violations
	// section renders nothing, never a 500.
	PushViolationsLog string

	// SweepStatusFile is the sweep's findings artifact (internal/queue.WriteSweepStatus) —
	// the board's RENDER surface (self-surfacing-diagnostics.md, Part 3): /health reads
	// it and renders per-project stalls + broken links, extending the push-violations
	// pattern. Optional; "" (the surface_board=false toggle, or simply unwired) or an
	// absent/unreadable file just means the section renders nothing, never a 500. Kept
	// as a stable file-based contract (a plain struct below), not an internal/queue
	// dependency — same convention as PushViolationsLog.
	SweepStatusFile string

	// IssuesRoot is the taxonomy's Issue level (issues/*.md) — optional; "" just
	// means /tree renders Milestone->Epic with no Issue level (and every task
	// parented to an issue instead falls into the orphan bucket), never a 500.
	IssuesRoot string

	// KnownRoles is the project's additional declared roles (e.g. roster Type
	// values) beyond roles.FixedRoles — fed to roles.CanonicalLane so the board's
	// lane chips can fold a legacy/instance lane's label to its canonical role
	// (role-keyed-lanes.md). Optional; nil just means only roles.FixedRoles'
	// instance names get folded, everything else reads as legacy — never a 500.
	KnownRoles []string
}

// pushViolationsShown caps how many of the log's most recent entries /health renders.
const pushViolationsShown = 20

// pushViolation is one push-guard deny record, read from PushViolationsLog. Kept as
// its own plain struct here (not shared with internal/hook) — the log is a stable
// file-based contract between the two packages, not a Go dependency; this package
// never depends on internal/hook.
type pushViolation struct {
	TS        string `json:"ts"`
	SessionID string `json:"session_id"`
	Repo      string `json:"repo"`
	Target    string `json:"target"`
	Command   string `json:"command"`
	Force     bool   `json:"force"`
	Reason    string `json:"reason"`
}

// readPushViolations reads path's JSONL entries and returns up to limit of the most
// recent ones, most-recent-first (push-guard only ever appends, so the last line in
// the file is the newest). An absent/unreadable/empty log, or a "" path, is not an
// error — just nothing to show; malformed individual lines are skipped rather than
// failing the whole read.
func readPushViolations(path string, limit int) []pushViolation {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var all []pushViolation
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v pushViolation
		if json.Unmarshal([]byte(line), &v) == nil {
			all = append(all, v)
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all
}

// sweepStall / sweepBroken / sweepStatus mirror internal/queue's SweepStatus JSON
// shape — decoded here rather than imported, the same file-based-contract convention
// pushViolation uses (this package never depends on internal/queue).
type sweepStall struct {
	Lane    string `json:"lane"`
	Pending int    `json:"pending"`
}

type sweepBroken struct {
	Lane  string   `json:"lane"`
	Links []string `json:"links"`
}

type sweepStatus struct {
	TS           string        `json:"ts"`
	Stalled      int           `json:"stalled"`
	Broken       int           `json:"broken"`
	StalledLanes []sweepStall  `json:"stalled_lanes"`
	BrokenLanes  []sweepBroken `json:"broken_lanes"`
	Milestone    string        `json:"milestone"`
	Present      bool          `json:"-"` // set by readSweepStatus, not from the file
}

// readSweepStatus reads the sweep findings artifact at path. An absent/unreadable/
// empty file, or a "" path, returns Present=false (nothing to render) — never an
// error, so /health can't 500 on it. A malformed file is likewise treated as absent.
func readSweepStatus(path string) sweepStatus {
	if path == "" {
		return sweepStatus{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sweepStatus{}
	}
	var s sweepStatus
	if json.Unmarshal(data, &s) != nil {
		return sweepStatus{}
	}
	s.Present = true
	return s
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleBoard)
	mux.HandleFunc("/store", s.handleStore)
	mux.HandleFunc("/task", s.handleTask)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/tree", s.handleTree)
	return mux
}

// handleTree renders the Milestone->Epic->Issue->Task hierarchy view (see tree.go).
// Best-effort by construction: a missing/empty milestones.yaml or issues/ just
// means an empty tree (HasHierarchy=false), never a 500 — the queue/store views
// stay fully independent of this one.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	data := BuildTree(s.Store, s.IssuesRoot, s.Root)
	if err := treeTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// base resolves which root a task-detail request reads from.
func (s *Server) base(src string) string {
	if src == "store" && s.Store != "" {
		return s.Store
	}
	return s.Root
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tasks, _ := Scan(s.Root)
	laneFilter := r.URL.Query().Get("lane")
	statusFilter := r.URL.Query().Get("status")
	ownerFilter := r.URL.Query().Get("owner")

	cols := map[string][]Task{}
	// Facet inventories + hide-empty counts are computed over ALL tasks (before the
	// active filter narrows the board) so switching filters stays possible — an
	// active lane filter must not zero out every OTHER lane's chip
	// (board-role-facets-hide-empty.md, Change 2). laneLive/ownerLive count LIVE
	// (non-done) tasks; statusTotal counts every task in that status (so a done chip
	// stays reachable when done tasks exist — the done COLUMN was always kept).
	laneSet := map[string]bool{}
	ownerBucketSet := map[string]bool{}
	laneLive := map[string]int{}
	ownerLive := map[string]int{}
	statusTotal := map[string]int{}
	matched := 0
	for _, t := range tasks {
		laneSet[t.Lane] = true
		live := !isDone(t.Status)
		if live {
			laneLive[t.Lane]++
		}
		statusTotal[t.Status]++
		bucket := ownerRoleBucket(t.Owner, s.KnownRoles)
		if bucket != "" {
			ownerBucketSet[bucket] = true
			if live {
				ownerLive[bucket]++
			}
		}
		if laneFilter != "" && t.Lane != laneFilter {
			continue
		}
		if statusFilter != "" && t.Status != statusFilter {
			continue
		}
		// Owner filter matches a task whose RAW owner equals the filter (individual
		// filterability preserved, cheap) OR whose canonical role bucket does (the
		// role-bucketed chips filter this way — one click covers every owner that
		// folds to that role).
		if ownerFilter != "" && t.Owner != ownerFilter && ownerRoleBucket(t.Owner, s.KnownRoles) != ownerFilter {
			continue
		}
		cols[t.Status] = append(cols[t.Status], t)
		matched++
	}
	var lanes, ownerBuckets []string
	for l := range laneSet {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)
	for b := range ownerBucketSet {
		ownerBuckets = append(ownerBuckets, b)
	}
	sort.Strings(ownerBuckets)

	statuses := append([]string{}, knownStatuses...)
	if len(cols["other"]) > 0 {
		statuses = append(statuses, "other")
	}

	data := boardData{
		Root: s.Root, Total: matched, HasStore: s.Store != "",
		LaneFilter: laneFilter, StatusFilter: statusFilter, OwnerFilter: ownerFilter,
		HasFilter: laneFilter != "" || statusFilter != "" || ownerFilter != "",
	}
	// Hide-empty (Change 2): a facet value with zero live/total tasks renders no
	// chip — EXCEPT the value the user is currently filtering on, which stays
	// visible even at count 0 so the active filter is always clearable.
	for _, l := range lanes {
		if laneLive[l] == 0 && laneFilter != l {
			continue
		}
		data.LaneChips = append(data.LaneChips, laneChip(l, laneChipLabel(l, s.KnownRoles), laneFilter, statusFilter, ownerFilter))
	}
	for _, st := range knownStatuses {
		if statusTotal[st] == 0 && statusFilter != st {
			continue
		}
		data.StatusChips = append(data.StatusChips, statusChip(st, laneFilter, statusFilter, ownerFilter))
	}
	for _, b := range ownerBuckets {
		if ownerLive[b] == 0 && ownerFilter != b {
			continue
		}
		data.OwnerChips = append(data.OwnerChips, ownerChip(b, laneFilter, statusFilter, ownerFilter))
	}
	for _, st := range statuses {
		data.Columns = append(data.Columns, column{Status: st, Tasks: cols[st], Collapsed: isDone(st)})
	}
	if err := boardTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// filterHref builds a board URL carrying whichever of lane/status/owner are
// non-empty — the plain <a href> every chip and the board's filter links are, so
// narrowing/clearing works with no JS.
func filterHref(lane, status, owner string) string {
	q := url.Values{}
	if lane != "" {
		q.Set("lane", lane)
	}
	if status != "" {
		q.Set("status", status)
	}
	if owner != "" {
		q.Set("owner", owner)
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// toggle is a chip's own dimension value: clicking an already-active chip clears it
// (toggle off), clicking any other one sets it — chips never combine within their own
// dimension, only across dimensions (AND).
func toggle(current, label string) string {
	if current == label {
		return ""
	}
	return label
}

// laneChip builds one lane filter chip: value is the raw lane dir name filtering
// and Active-matching still key on (unchanged by role-grouped display — see
// laneChipLabel), displayLabel is what's actually rendered.
func laneChip(value, displayLabel, lane, status, owner string) chip {
	return chip{Label: displayLabel, Active: lane == value, Href: filterHref(toggle(lane, value), status, owner)}
}

func statusChip(label, lane, status, owner string) chip {
	return chip{Label: label, Active: status == label, Href: filterHref(lane, toggle(status, label), owner)}
}

// ownerChip builds one owner filter chip. Since board-role-facets-hide-empty.md
// (Change 1) the owner facet is keyed by CANONICAL ROLE, so label here is a role
// bucket (or legacyLaneGroup), not a raw owner instance — clicking it filters to
// every task whose owner folds to that role (see handleBoard's owner match).
func ownerChip(label, lane, status, owner string) chip {
	return chip{Label: label, Active: owner == label, Href: filterHref(lane, status, toggle(owner, label))}
}

// handleHealth runs the audit live and renders findings grouped by category.
// Resilient by construction: DiskLens/SessionLens errors (a missing/misconfigured
// path) just mean fewer findings, never a 500 — an audit page that can't 500 is the
// point of it being read-only.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var findings []audit.Finding
	var sessionCounts map[string]audit.SessionCounts

	if s.Root != "" && s.Store != "" {
		if f, err := audit.DiskLens(s.Root, s.Store, s.CanonicalStatuses); err == nil {
			findings = append(findings, f...)
		}
	}
	if s.SessionTasksRoot != "" {
		cfg := audit.WorkishConfig{DescriptionLengthThreshold: s.WorkishThreshold, Markers: s.WorkishMarkers}
		if f, counts, err := audit.SessionLens(s.SessionTasksRoot, "", cfg); err == nil {
			findings = append(findings, f...)
			sessionCounts = counts
		}
	}

	byCategory := map[string][]audit.Finding{}
	for _, f := range findings {
		byCategory[f.Category] = append(byCategory[f.Category], f)
	}

	data := healthData{
		HasStore: s.Store != "", Summary: healthSummaryLine(findings),
		PushViolations: readPushViolations(s.PushViolationsLog, pushViolationsShown),
		SweepStatus:    readSweepStatus(s.SweepStatusFile),
	}
	for _, cat := range audit.Categories {
		group := byCategory[cat]
		if len(group) == 0 {
			continue
		}
		hg := healthGroup{Category: cat, Count: len(group)}
		for _, f := range group {
			hg.Findings = append(hg.Findings, healthFinding{Finding: f, Href: healthFindingHref(f)})
		}
		data.Groups = append(data.Groups, hg)
	}

	var sessionNames []string
	for n := range sessionCounts {
		sessionNames = append(sessionNames, n)
	}
	sort.Strings(sessionNames)
	for _, n := range sessionNames {
		data.SessionCounts = append(data.SessionCounts, sessionCountRow{Name: n, Counts: sessionCounts[n]})
	}

	if err := healthTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// healthFindingHref is a Finding's /task link, or "" when it names no single file
// (duplicate-id) or none at all (session-misuse).
func healthFindingHref(f audit.Finding) string {
	if f.RefPath == "" {
		return ""
	}
	if f.RefSrc == "" {
		return "/task?p=" + url.QueryEscape(f.RefPath)
	}
	return "/task?src=" + url.QueryEscape(f.RefSrc) + "&p=" + url.QueryEscape(f.RefPath)
}

// healthSummaryLine matches `leadkit audit`'s one-line summary format, so the two
// surfaces read the same way.
func healthSummaryLine(findings []audit.Finding) string {
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Category]++
	}
	return fmt.Sprintf("%d findings — %d orphaned-active, %d dangling, %d drift, %d dup-id, %d noncanonical, %d session-misuse",
		len(findings),
		counts[audit.CategoryOrphanedActive], counts[audit.CategoryDanglingSymlink], counts[audit.CategoryStatusDrift],
		counts[audit.CategoryDuplicateID], counts[audit.CategoryNoncanonicalStatus], counts[audit.CategorySessionMisuse],
	)
}

// handleStore renders the canonical store grouped by frontmatter status.
func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	if s.Store == "" {
		http.NotFound(w, r)
		return
	}
	tasks, _ := ScanStore(s.Store)
	statusFilter := r.URL.Query().Get("status")

	cols := map[string][]Task{}
	order := []string{}
	for _, t := range tasks {
		if statusFilter != "" && t.Status != statusFilter {
			continue
		}
		if _, ok := cols[t.Status]; !ok {
			order = append(order, t.Status)
		}
		cols[t.Status] = append(cols[t.Status], t)
	}
	// Stable status order: known active first, then the rest (incl completed) sorted.
	sort.Slice(order, func(i, j int) bool {
		ri, rj := statusRank(order[i]), statusRank(order[j])
		if ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})

	data := boardData{Root: s.Store, Total: len(tasks), IsStore: true}
	for _, st := range order {
		data.Columns = append(data.Columns, column{Status: st, Tasks: cols[st], Collapsed: isDone(st)})
	}
	if err := boardTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func statusRank(s string) int {
	for i, k := range knownStatuses {
		if k == s {
			return i
		}
	}
	if isDone(s) {
		return len(knownStatuses) + 1
	}
	return len(knownStatuses) // unknowns just before done
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("p")
	src := r.URL.Query().Get("src")
	base := s.base(src)
	// Guard against path traversal: only allow paths that stay within base.
	clean := filepath.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		http.Error(w, "bad path", 400)
		return
	}
	full := filepath.Join(base, clean)
	b, err := os.ReadFile(full)
	if err != nil {
		// Queue entries are often symlinks into a canonical tasks/ dir; a dangling
		// one (stale/removed target) shouldn't 404 the page — show it as unreadable.
		if _, lerr := os.Lstat(full); lerr == nil {
			tgt, _ := os.Readlink(full)
			data := taskData{Rel: clean, Src: src, Lane: filepath.Base(filepath.Dir(filepath.Dir(full))),
				Status: filepath.Base(filepath.Dir(full)), Title: filepath.Base(full),
				BodyHTML: template.HTML(fmt.Sprintf("<p>⚠ unreadable: %s</p><p class=muted>symlink → %s</p>",
					html.EscapeString(err.Error()), html.EscapeString(tgt)))}
			_ = taskTmpl.Execute(w, data)
			return
		}
		http.NotFound(w, r)
		return
	}
	meta, keys, body := parseFrontmatter(string(b))
	statusDir := filepath.Base(filepath.Dir(full))
	lane := filepath.Base(filepath.Dir(filepath.Dir(full)))
	data := taskData{
		Rel: clean, Src: src, Lane: lane, Status: statusDir,
		Title: deriveTitle(meta, body, full), Meta: meta, MetaKeys: keys,
		BodyHTML: template.HTML(renderMarkdown(body)),
	}
	if err := taskTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// renderMarkdown is a deliberately tiny md→html pass (headings, fenced code,
// inline code, bold) — enough to read a task without pulling in a markdown lib.
// Everything else is escaped and preserved, so it degrades to readable text.
func renderMarkdown(md string) string {
	var b strings.Builder
	inCode := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inCode {
				b.WriteString("</pre>\n")
			} else {
				b.WriteString("<pre class=code>")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString(html.EscapeString(line) + "\n")
			continue
		}
		switch {
		case strings.HasPrefix(line, "### "):
			b.WriteString("<h3>" + inline(line[4:]) + "</h3>\n")
		case strings.HasPrefix(line, "## "):
			b.WriteString("<h2>" + inline(line[3:]) + "</h2>\n")
		case strings.HasPrefix(line, "# "):
			b.WriteString("<h1>" + inline(line[2:]) + "</h1>\n")
		case strings.TrimSpace(line) == "":
			b.WriteString("<br>\n")
		default:
			b.WriteString("<div>" + inline(line) + "</div>\n")
		}
	}
	if inCode {
		b.WriteString("</pre>\n")
	}
	return b.String()
}

func inline(s string) string {
	s = html.EscapeString(s)
	// inline `code`
	for strings.Count(s, "`") >= 2 {
		s = strings.Replace(s, "`", "<code>", 1)
		s = strings.Replace(s, "`", "</code>", 1)
	}
	// **bold**
	for strings.Count(s, "**") >= 2 {
		s = strings.Replace(s, "**", "<b>", 1)
		s = strings.Replace(s, "**", "</b>", 1)
	}
	return s
}

// ListenAndServe starts the read-only viewer from an already-populated Server (see
// its fields — Store and SessionTasksRoot may both be "" to disable /store and the
// session lens on /health, respectively).
func ListenAndServe(s *Server, addr string) error {
	fmt.Printf("leadkit viewer: serving queue=%s store=%q at http://%s (read-only)\n", s.Root, s.Store, addr)
	return http.ListenAndServe(addr, s.Routes())
}
