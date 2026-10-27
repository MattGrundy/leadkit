package hierarchy

// `hierarchy cluster-issues` proposes the missing middle of Task -> Issue -> Epic:
// within each Epic (from the internal Task->Epic proposal — reusing propose.go's
// Propose, so type inference + keyword/id-range signals are shared, never
// re-derived), cluster that Epic's Tasks into candidate Issues by subject-theme.
// READ-ONLY — writes nothing (no issues/ files, no parent: tags); applying a
// reviewed candidate is a later, separately-guarded migration, same shape as
// propose.go's own "suggests, never assigns" stance.

import (
	"math"
	"sort"
	"strings"
)

// ClusterCandidate is one candidate Issue proposed within an Epic — a suggestion,
// never applied here.
type ClusterCandidate struct {
	Epic          string   `json:"epic"`
	SuggestedName string   `json:"suggested_name"`
	TaskIDs       []string `json:"task_ids"`
	Count         int      `json:"count"`
	SharedTokens  []string `json:"shared_tokens"`
	DoneCount     int      `json:"done_count"`
	ActiveCount   int      `json:"active_count"`
	// TaskTitles is populated only for the --epic DETAIL view (id -> title) — the
	// summary view never needs per-task titles, so Summary rows stay small even
	// across all ~24 epics.
	TaskTitles map[string]string `json:"task_titles,omitempty"`
}

// EpicClusterSummary is cluster-issues' default/--json SUMMARY row: one Epic's
// candidate-Issue counts + sizes, done vs active — small enough to read across all
// epics at once.
type EpicClusterSummary struct {
	Epic       string `json:"epic"`
	Candidates int    `json:"candidates"`
	Sizes      []int  `json:"sizes"` // candidate sizes, descending
	Done       int    `json:"done"`
	Active     int    `json:"active"`
}

// ClusterResult is `hierarchy cluster-issues`' output — exactly one of Summary
// (default) or Detail (--epic) is populated, per the spec's two output modes.
type ClusterResult struct {
	Summary []EpicClusterSummary `json:"summary,omitempty"`
	Detail  []ClusterCandidate   `json:"detail,omitempty"`
	// NeedsEpic is how many Tasks propose.Propose itself proposed future-work for
	// (no real Epic signal) — these are reported "needs an epic first", never
	// clustered (clustering a Task nobody has even proposed an Epic for would be
	// guessing on top of a guess).
	NeedsEpic int `json:"needs_epic"`
}

// ClusterIssues runs the Task->Epic proposal internally (Propose, all ~1400 tasks,
// done+active — same default as `hierarchy propose`) and, within each proposed
// Epic, clusters its Tasks into candidate Issues by subject-theme. epicFilter
// switches to DETAIL mode for just that epic (empty = SUMMARY across every epic).
// minSize is the minimum shared-token frequency that forms a real multi-task
// cluster (below it, a task becomes its own singleton candidate) — the spec's
// `--min`, default 2. hints are passed straight through to Propose (the adopter's
// optional curated type/keyword seeds; empty by default).
func ClusterIssues(model Model, subtasks []Subtask, epicFilter string, minSize int, hints Hints) ClusterResult {
	if minSize < 2 {
		minSize = 2
	}
	subtaskByID := make(map[string]Subtask, len(subtasks))
	for _, s := range subtasks {
		subtaskByID[s.ID] = s
	}

	proposed := Propose(model, subtasks, false, hints)
	byEpic := map[string][]taskItem{}
	needsEpic := 0
	for _, p := range proposed.Proposals {
		if p.ProposedParent == FutureWorkEpic {
			needsEpic++
			continue
		}
		s, ok := subtaskByID[p.ID]
		if !ok {
			continue
		}
		byEpic[p.ProposedParent] = append(byEpic[p.ProposedParent], taskItem{Subtask: s, tokens: tokenizeTitle(s.Title)})
	}

	epicIDs := make([]string, 0, len(byEpic))
	for e := range byEpic {
		epicIDs = append(epicIDs, e)
	}
	sort.Strings(epicIDs)

	res := ClusterResult{NeedsEpic: needsEpic}
	for _, epic := range epicIDs {
		if epicFilter != "" && epic != epicFilter {
			continue
		}
		items := append([]taskItem(nil), byEpic[epic]...)
		sort.Slice(items, func(i, j int) bool {
			ni, nj := idNum(items[i].ID), idNum(items[j].ID)
			if ni != nj {
				return ni < nj
			}
			return items[i].ID < items[j].ID
		})
		candidates := clusterEpicTasks(epic, items, minSize)

		if epicFilter != "" {
			res.Detail = append(res.Detail, candidates...)
			continue
		}
		sizes := make([]int, len(candidates))
		done, active := 0, 0
		for i, c := range candidates {
			sizes[i] = c.Count
			done += c.DoneCount
			active += c.ActiveCount
		}
		sort.Sort(sort.Reverse(sort.IntSlice(sizes)))
		res.Summary = append(res.Summary, EpicClusterSummary{Epic: epic, Candidates: len(candidates), Sizes: sizes, Done: done, Active: active})
	}
	return res
}

// taskItem is one Task carrying its pre-tokenized title — computed once per task,
// reused across every clustering round within its epic.
type taskItem struct {
	Subtask
	tokens []string
}

// clusterEpicTasks greedily clusters items by their most frequent shared token: each
// round picks the surviving token shared by the most tasks (>= minSize), ties broken
// by the TIGHTEST id range among its members (id-adjacency — the spec's "weak
// secondary signal": same theme + near ids reinforce, never overrides theme).
// Whatever no shared token (>= minSize) can group becomes singleton candidates.
func clusterEpicTasks(epic string, items []taskItem, minSize int) []ClusterCandidate {
	remaining := append([]taskItem(nil), items...)
	var candidates []ClusterCandidate

	for len(remaining) > 0 {
		freq := map[string]int{}
		for _, t := range remaining {
			for _, tok := range uniqueStrings(t.tokens) {
				freq[tok]++
			}
		}
		toks := make([]string, 0, len(freq))
		for tok := range freq {
			toks = append(toks, tok)
		}
		sort.Strings(toks) // deterministic base order before the frequency/span comparison

		bestTok := ""
		bestCount := 0
		bestSpan := math.MaxInt
		for _, tok := range toks {
			count := freq[tok]
			if count < minSize {
				continue
			}
			span := idSpanForToken(remaining, tok)
			if count > bestCount || (count == bestCount && span < bestSpan) {
				bestTok, bestCount, bestSpan = tok, count, span
			}
		}
		if bestTok == "" {
			for _, t := range remaining {
				candidates = append(candidates, singletonCandidate(epic, t))
			}
			break
		}

		var member, rest []taskItem
		for _, t := range remaining {
			if containsString(t.tokens, bestTok) {
				member = append(member, t)
			} else {
				rest = append(rest, t)
			}
		}
		candidates = append(candidates, buildCandidate(epic, member))
		remaining = rest
	}
	return candidates
}

// idSpanForToken is tok's id-adjacency tightness among items that carry it (max id
// - min id) — smaller means those tasks were filed closer together, the spec's weak
// secondary signal for breaking a frequency tie between two equally-plausible themes.
func idSpanForToken(items []taskItem, tok string) int {
	min, max := math.MaxInt, math.MinInt
	for _, t := range items {
		if !containsString(t.tokens, tok) {
			continue
		}
		n := idNum(t.ID)
		if n < min {
			min = n
		}
		if n > max {
			max = n
		}
	}
	if min > max {
		return 0
	}
	return max - min
}

// buildCandidate finalizes one multi-task cluster: its suggested name (the shared
// tokens, in the first member's own title order — see suggestedName), sorted task
// ids, and the done/active split.
func buildCandidate(epic string, member []taskItem) ClusterCandidate {
	tokenCounts := map[string]int{}
	for _, t := range member {
		for _, tok := range uniqueStrings(t.tokens) {
			tokenCounts[tok]++
		}
	}
	shared := map[string]bool{}
	var sharedList []string
	for tok, c := range tokenCounts {
		if c == len(member) {
			shared[tok] = true
			sharedList = append(sharedList, tok)
		}
	}
	sort.Strings(sharedList)

	ids := make([]string, len(member))
	titles := make(map[string]string, len(member))
	done, active := 0, 0
	for i, t := range member {
		ids[i] = t.ID
		titles[t.ID] = t.Title
		if terminalStatuses[normalizeStatus(t.Status)] {
			done++
		} else {
			active++
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		ni, nj := idNum(ids[i]), idNum(ids[j])
		if ni != nj {
			return ni < nj
		}
		return ids[i] < ids[j]
	})

	return ClusterCandidate{
		Epic: epic, SuggestedName: suggestedName(member, shared), TaskIDs: ids,
		Count: len(member), SharedTokens: sharedList, DoneCount: done, ActiveCount: active,
		TaskTitles: titles,
	}
}

// singletonCandidate is a 1-task candidate Issue — a lone task with no (or too-weak)
// shared theme with anything else remaining in its epic. Its suggested name falls
// back to its own tokens (capped) since there's no cluster-mate to confirm a theme.
func singletonCandidate(epic string, t taskItem) ClusterCandidate {
	name := strings.Join(capTokens(t.tokens, 3), "-")
	if name == "" {
		name = t.Title
	}
	done, active := 0, 1
	if terminalStatuses[normalizeStatus(t.Status)] {
		done, active = 1, 0
	}
	return ClusterCandidate{
		Epic: epic, SuggestedName: name, TaskIDs: []string{t.ID}, Count: 1,
		SharedTokens: capTokens(t.tokens, 3), DoneCount: done, ActiveCount: active,
		TaskTitles: map[string]string{t.ID: t.Title},
	}
}

// suggestedName names a multi-task candidate from its shared tokens, ordered as
// they appear in the first member's own title (not alphabetically) — reads more
// like a real stem ("registry-cache") than a sorted word-bag.
func suggestedName(member []taskItem, shared map[string]bool) string {
	if len(member) == 0 {
		return ""
	}
	var ordered []string
	seen := map[string]bool{}
	for _, tok := range member[0].tokens {
		if shared[tok] && !seen[tok] {
			ordered = append(ordered, tok)
			seen[tok] = true
		}
	}
	if len(ordered) == 0 {
		return member[0].tokens[0]
	}
	if len(ordered) > 3 {
		ordered = ordered[:3]
	}
	return strings.Join(ordered, "-")
}

// stopWordsForClustering are dropped when tokenizing a title — filler words that
// never distinguish one theme from another. Deliberately separate from any other
// package's stopword list (none currently exists) — this codebase's small-package
// decoupling convention.
var stopWordsForClustering = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
	"fix": true, "add": true, "update": true, "updates": true, "support": true,
	"issue": true, "task": true, "work": true, "not": true, "when": true, "that": true,
}

// tokenizeTitle splits title into lowercase significant tokens (alnum runs),
// dropping stopwords, pure-numeric tokens (ids/PR numbers carry no thematic
// signal), and anything shorter than 3 chars.
func tokenizeTitle(title string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		w := cur.String()
		cur.Reset()
		if len(w) < 3 || stopWordsForClustering[w] || isAllDigits(w) {
			return
		}
		out = append(out, w)
	}
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func uniqueStrings(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func capTokens(tokens []string, n int) []string {
	if len(tokens) > n {
		return append([]string(nil), tokens[:n]...)
	}
	return append([]string(nil), tokens...)
}
