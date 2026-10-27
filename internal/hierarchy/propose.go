package hierarchy

// `hierarchy propose` is READ-ONLY — it suggests, never assigns. Applying a
// proposal (writing `parent:` onto a task) is a later, separately-guarded migration
// (same --apply --confirm shape as `type` backfill) — this package writes nothing,
// ever. Every rule below is explicit and testable, same "no model judgement inside"
// stance as internal/orchestrate's planner.
//
// CONSERVATIVE BY DESIGN: an epic is only ever proposed on a real per-task signal —
// an inferred type match and/or a subject-keyword match. id-adjacency is a WEAK
// TIEBREAKER between two candidates a task's OWN type signal already made plausible
// (see nearestCandidateMatch) — it never gap-fills a signal-less task from a
// neighbor. An earlier version did exactly that (a nearest-any-signaled-neighbor
// pass), and it snowballed: every future-work task with no real signal inherited
// whichever epic happened to have the nearest keyword hit, collapsing ~1400 tasks
// onto 2-3 dominant epics. A task with no real signal stays future-work — an honest
// "needs human review", not a false-confidence guess.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Confidence levels, in ascending order of certainty.
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// FutureWorkEpic is the catch-all target for a subtask that fits nothing
// migration-related, OR that simply carries no real signal either way — never
// force-fit into an unrelated epic. See ProposeResult.FutureWork: this is the
// "needs human review" count.
const FutureWorkEpic = "future-work"

// KeywordHint maps subject keywords to an epic id — matched case-insensitively as a
// substring against a subtask's title. Adopter-supplied (leadkit.yaml's
// hierarchy.keyword_hints); source ships none.
type KeywordHint struct {
	Keywords []string
	Epic     string
}

// Hints is the OPTIONAL, adopter-supplied curated hint tables Propose consults on top
// of its generic model-driven alias/title match. Both maps are empty in a zero Hints —
// leadkit SOURCE bakes in no project-specific epic names; a project sets these in its
// leadkit.yaml (config.Hierarchy, translated by the caller). Type maps a canonical
// agent type to its likely epic id(s); Keyword maps title keywords to an epic id.
// A hint naming an epic id absent from the parsed model is simply skipped, never
// invented — so an empty Hints degrades cleanly to "generic alias/title match only".
type Hints struct {
	Type    map[string][]string
	Keyword []KeywordHint
}

// Proposal is one subtask's suggested epic — a suggestion, never applied here.
type Proposal struct {
	ID             string `json:"id"`
	ProposedParent string `json:"proposed_parent"`
	Confidence     string `json:"confidence"`
	Why            string `json:"why"`
}

// ProposeResult is `hierarchy propose`'s full output.
type ProposeResult struct {
	Proposals  []Proposal     `json:"proposals"`
	ByEpic     map[string]int `json:"by_epic"`
	High       int            `json:"high"`
	Medium     int            `json:"medium"`
	Low        int            `json:"low"`
	FutureWork int            `json:"future_work"` // == "needs human review": no real signal matched
}

// signal is pass 1's raw findings for one subtask, before finalize() turns it into a
// Proposal — kept around (rather than collapsed immediately) so the tiebreak pass
// can still see an ambiguous type match's FULL candidate list.
type signal struct {
	id        string
	kwEpic    string
	kwWhy     string
	typeEpics []string
	typeWhy   string
}

// Propose suggests a best-fit epic for every unmapped subtask (Parent == "" or
// pointing at an id unknown to model) — ALL of them by default (done/archived
// included, so the rollup can later show "14/15 done" honestly); activeOnly skips
// terminal-status subtasks. Writes nothing — see the package doc comment. hints are
// the adopter's OPTIONAL curated type/keyword seeds (config.Hierarchy); a zero Hints
// means "generic model-driven alias/title match only" — leadkit source bakes in none.
func Propose(model Model, subtasks []Subtask, activeOnly bool, hints Hints) ProposeResult {
	known := model.KnownIDs()

	var unmapped []Subtask
	for _, s := range subtasks {
		if s.Parent != "" && known[s.Parent] {
			continue // already mapped — not this command's job
		}
		if activeOnly && terminalStatuses[normalizeStatus(s.Status)] {
			continue
		}
		unmapped = append(unmapped, s)
	}
	// Stable id order (numeric where possible) — deterministic output, and lets the
	// tiebreak pass below look at genuinely adjacent neighbors.
	sort.Slice(unmapped, func(i, j int) bool {
		ni, nj := idNum(unmapped[i].ID), idNum(unmapped[j].ID)
		if ni != nj {
			return ni < nj
		}
		return unmapped[i].ID < unmapped[j].ID
	})

	signals := make([]signal, len(unmapped))
	proposals := make([]Proposal, len(unmapped))
	for i, s := range unmapped {
		signals[i] = computeSignal(s, model, known, hints)
		proposals[i] = finalize(signals[i])
	}
	// Tiebreak pass: ONLY for a task whose type signal named 2+ plausible candidate
	// epics with no keyword to pick between them — id-adjacency picks whichever
	// candidate a nearby neighbor's OWN (already-signaled) proposal agrees with. This
	// never touches a future-work (no-signal) proposal — see the package doc comment
	// on why that gap-fill was removed.
	for i, sig := range signals {
		if sig.kwEpic != "" || len(sig.typeEpics) < 2 {
			continue
		}
		if epic, ok := nearestCandidateMatch(unmapped, proposals, i, sig.typeEpics); ok {
			proposals[i] = Proposal{
				ID: sig.id, ProposedParent: epic, Confidence: ConfidenceMedium,
				Why: sig.typeWhy + fmt.Sprintf(" (id-range tiebreak among ambiguous candidates: %s)", strings.Join(sig.typeEpics, ", ")),
			}
		}
	}

	res := ProposeResult{Proposals: proposals, ByEpic: map[string]int{}}
	for _, p := range proposals {
		res.ByEpic[p.ProposedParent]++
		switch p.Confidence {
		case ConfidenceHigh:
			res.High++
		case ConfidenceMedium:
			res.Medium++
		default:
			res.Low++
		}
		if p.ProposedParent == FutureWorkEpic {
			res.FutureWork++
		}
	}
	return res
}

// computeSignal gathers keyword + type findings only — no decision yet (finalize
// decides; the tiebreak pass needs the raw candidate list too).
func computeSignal(s Subtask, model Model, known map[string]bool, hints Hints) signal {
	kwEpic, kwWhy := matchKeyword(s.Title, model, known, hints)
	typeEpics, typeWhy := matchType(s.Type, known, hints)
	return signal{id: s.ID, kwEpic: kwEpic, kwWhy: kwWhy, typeEpics: typeEpics, typeWhy: typeWhy}
}

// finalize turns one signal into a Proposal: keyword match (if it agrees with, or
// type gives no hint, wins HIGH; if it disagrees with type, still MEDIUM — a keyword
// hit is strong evidence) then type match (MEDIUM if it names exactly one candidate
// epic, else LOW pending the tiebreak pass). No match at all -> future-work (LOW) —
// a real "no signal", never gap-filled from elsewhere.
func finalize(sig signal) Proposal {
	switch {
	case sig.kwEpic != "" && (len(sig.typeEpics) == 0 || contains(sig.typeEpics, sig.kwEpic)):
		return Proposal{ID: sig.id, ProposedParent: sig.kwEpic, Confidence: ConfidenceHigh, Why: sig.kwWhy}
	case sig.kwEpic != "":
		return Proposal{ID: sig.id, ProposedParent: sig.kwEpic, Confidence: ConfidenceMedium, Why: sig.kwWhy + " (type hint disagreed, keyword wins)"}
	case len(sig.typeEpics) == 1:
		return Proposal{ID: sig.id, ProposedParent: sig.typeEpics[0], Confidence: ConfidenceMedium, Why: sig.typeWhy}
	case len(sig.typeEpics) > 1:
		return Proposal{ID: sig.id, ProposedParent: sig.typeEpics[0], Confidence: ConfidenceLow, Why: sig.typeWhy + fmt.Sprintf(" (ambiguous: also %s)", strings.Join(sig.typeEpics[1:], ", "))}
	default:
		return Proposal{ID: sig.id, ProposedParent: FutureWorkEpic, Confidence: ConfidenceLow, Why: "no type/keyword signal matched"}
	}
}

// matchKeyword checks the adopter's configured keyword hints first (a project's own
// hand-picked, high-confidence signals from leadkit.yaml — empty by default), then
// falls through to a GENERIC, model-driven check: does title (case/separator-
// insensitive) contain any epic's own id, title, or one of its optional `aliases:`
// entries, verbatim? An epic with no aliases behaves exactly as before (its id/title
// alone rarely appear literally in a task's phrasing, by design — see
// epicMatchKeywords' own doc comment for why aliases, not id/title-token-splitting,
// are what actually un-strands an epic whose tasks reference it only by a PR number).
func matchKeyword(title string, model Model, known map[string]bool, hints Hints) (epic, why string) {
	t := strings.ToLower(title)
	for _, h := range hints.Keyword {
		if h.Epic != FutureWorkEpic && !known[h.Epic] {
			continue // the hint's epic isn't in THIS model — never invent one
		}
		for _, kw := range h.Keywords {
			if strings.Contains(t, strings.ToLower(kw)) {
				return h.Epic, fmt.Sprintf("keyword match: %q -> %s", kw, h.Epic)
			}
		}
	}

	normTitle := normalizeForMatch(title)
	for _, ms := range model.Milestones {
		for _, e := range ms.Epics {
			for _, kw := range epicMatchKeywords(e) {
				normKW := normalizeForMatch(kw)
				if normKW == "" {
					continue
				}
				if strings.Contains(normTitle, normKW) {
					return e.ID, fmt.Sprintf("alias/title match: %q -> %s", kw, e.ID)
				}
			}
		}
	}
	return "", ""
}

// epicMatchKeywords is one epic's full set of keyword candidates: its own id, its
// Note/Desc (milestones.yaml epics have no separate "title" field — these are the
// closest thing to one), and any `aliases:` entries — matched as WHOLE strings
// (never split into individual words), so a generic word inside a long id/note
// ("remove-legacy-nexus" containing "remove") can't accidentally claim unrelated
// tasks. This is deliberately conservative: an epic id/note rarely appears
// literally in a task's own phrasing, so that matching mostly stays a no-op —
// aliases (explicit, curated, one keyword per entry, e.g. a bare PR number like
// "18883") are the real un-stranding mechanism, and they're opt-in per epic.
func epicMatchKeywords(e Epic) []string {
	var out []string
	if e.ID != "" {
		out = append(out, e.ID)
	}
	if e.Note != "" {
		out = append(out, e.Note)
	}
	if e.Desc != "" {
		out = append(out, e.Desc)
	}
	out = append(out, e.Aliases...)
	return out
}

// normalizeForMatch lowercases s and folds "_"/" " to "-" — the same case/
// separator-insensitive comparison typeinfer.normalize already uses, so an alias
// like "ARGO_PR_CHECK" matches a title phrasing it "argo pr check" or "argo-pr-check"
// equally.
func normalizeForMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

func matchType(typ string, known map[string]bool, hints Hints) (epics []string, why string) {
	if typ == "" {
		return nil, ""
	}
	th, ok := hints.Type[typ]
	if !ok {
		return nil, ""
	}
	for _, e := range th {
		if known[e] {
			epics = append(epics, e)
		}
	}
	if len(epics) == 0 {
		return nil, ""
	}
	return epics, fmt.Sprintf("type match: %s -> %s", typ, epics[0])
}

// idRangeWindow bounds the tiebreak pass's search for a neighbor to agree with —
// "adjacent ids tend to share an epic" is real for genuinely nearby work (the same
// PR/spike often files a short run of consecutive ids), but unbounded it would reach
// across the whole store. Deliberately small and named so it's easy to tune.
const idRangeWindow = 5

// nearestCandidateMatch looks outward from index i (alternating +1/-1 steps, capped
// at idRangeWindow) for the closest OTHER subtask whose OWN finalized proposal is
// non-future-work AND is one of candidates — i.e. a neighbor that INDEPENDENTLY
// arrived at one of this task's already-plausible epics. It never considers a
// neighbor's proposal that isn't in candidates (that would be inventing a new
// candidate, not tiebreaking between the real ones), and it never fires for a task
// with no candidates at all (callers only call this when len(candidates) >= 2).
func nearestCandidateMatch(subtasks []Subtask, proposals []Proposal, i int, candidates []string) (string, bool) {
	for d := 1; d <= idRangeWindow; d++ {
		if i-d >= 0 && idNum(subtasks[i].ID)-idNum(subtasks[i-d].ID) <= idRangeWindow {
			if p := proposals[i-d].ProposedParent; p != FutureWorkEpic && contains(candidates, p) {
				return p, true
			}
		}
		if i+d < len(subtasks) && idNum(subtasks[i+d].ID)-idNum(subtasks[i].ID) <= idRangeWindow {
			if p := proposals[i+d].ProposedParent; p != FutureWorkEpic && contains(candidates, p) {
				return p, true
			}
		}
	}
	return "", false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// idNum parses a subtask id's leading digits for numeric id-range ordering; a
// non-numeric id (rare) sorts as 0, keeping it stable rather than erroring.
func idNum(id string) int {
	n, err := strconv.Atoi(id)
	if err != nil {
		return 0
	}
	return n
}
