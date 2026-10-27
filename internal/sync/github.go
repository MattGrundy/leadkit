package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mgpp.dev/leadkit/internal/hierarchy"
)

// The GitHub output: a ONE-WAY MIRROR, files -> GitHub issues.
//
// DIRECTION. leadkit's files stay authoritative. The mirror writes the issue to match
// the ticket and never the other way: a title, label or open/closed state edited on
// GitHub is overwritten on the next run (a closed issue whose ticket is not terminal is
// reopened). The remote is never read to change a ticket. Read-back is a different
// feature with its own conflict rules (ticket 148) and nothing here anticipates it
// except that the executor already holds the remote issue list it would need.
//
// WHAT SYNCS, and why so little. Per ticket, one issue:
//   - title, as "[<ticket id>] <title>" (the id keeps a GitHub number from being read as
//     a ticket id, and a ticket id from being read as a GitHub number);
//   - labels type:<type>, priority:<P>, status:<status> — leadkit OWNS labels carrying
//     those three prefixes and replaces them to match; every other label is left alone;
//   - state: closed when the ticket's status is terminal (done/completed -> completed;
//     dropped/superseded -> not planned), open otherwise.
//
// NOT synced, on purpose: `blocked_on` (free text, not a reference to anything GitHub
// could link, and the field most likely to describe an unfixed defect); owner, parent,
// milestone and epic (no GitHub equivalent that would stay exactly right); and the
// ticket BODY unless the caller opts in (see GitHubExecutor.Body). Fewer mappings that
// are exactly right beat a complete-looking set that drifts.
//
// SECURITY DEFAULT. Bodies are the part of a ticket most likely to describe an unfixed,
// exploitable defect, and a published issue cannot be unpublished. So the default is
// title + metadata only, and the safe thing is what happens when nobody decides.
// There is deliberately no scrubber and no attempt to classify tickets as sensitive.
// Titles are NOT guaranteed safe either: `sync run` prints every title it would
// publish, and a dry run is the default, so the owner sees them before they leave.

// githubTracker is this output's key in a ticket's `trackers:` map. Its value is
// "<owner>/<repo>#<number>" — the repo is part of the id so that pointing the mirror at a
// different repo can never be mistaken for the numbers already recorded (issue 12 of
// repo A is not issue 12 of repo B).
const githubTracker = "github"

// markerPrefix/markerSuffix wrap the stable identity written into every mirrored issue's body. It is what
// lets a run that died between creating an issue and recording its number find that
// issue again instead of creating a second one: the remote carries the ticket id, so
// the idempotency does not depend on the local write having happened. (Linear's
// executor has the window; this closes it.) Anyone who can edit the issue can remove
// or forge it; forging it can only make the mirror adopt and then overwrite the title
// and labels of an issue that claims to be a ticket.
const markerPrefix = "<!-- leadkit-sync:ticket="
const markerSuffix = " -->"

var markerRe = regexp.MustCompile(`<!-- leadkit-sync:ticket=([^\s]+) -->`)

func marker(localID string) string { return markerPrefix + localID + markerSuffix }

// metadataBody is the body of every issue when bodies are not opted in — fixed text, so
// it never drifts and never needs patching.
func metadataBody(localID string) string {
	return marker(localID) + "\n\nMirror of leadkit ticket " + localID + ". Title and metadata only; the ticket body is not synced.\n"
}

// fullBody is the opted-in body: the marker, then the ticket's own text.
func fullBody(localID, text string) string {
	const max = 60000 // GitHub's limit is 65536 characters
	if len(text) > max {
		text = text[:max] + "\n\n[truncated]\n"
	}
	return marker(localID) + "\n\n" + text
}

// GitHubAdapter is the Output implementation for GitHub. It mirrors tickets (the
// flat tasks/ store); see BuildTicketModel. It has no repo field: Plan is pure and
// repo-independent, and the repo only matters to the executor, which checks every
// recorded id against it.
type GitHubAdapter struct{}

func (GitHubAdapter) Name() string { return githubTracker }

// FlatTickets marks the adapter as taking BuildTicketModel's flat model, not
// BuildSyncModel's five-level tree. See BuildModelFor.
func (GitHubAdapter) FlatTickets() {}

// Plan emits one Op per ticket: create when no trackers.github is recorded, update (or
// close, for a terminal ticket) when one is. Like Linear's Plan it diffs only against
// the LOCAL record; whether the recorded issue still exists, or already matches, is the
// executor's question (it lists the repo once) because only the remote can answer it.
func (GitHubAdapter) Plan(model Model) ([]Op, error) {
	ops := make([]Op, 0, len(model.Nodes))
	for _, n := range model.Nodes {
		if n.Level != "task" {
			return nil, fmt.Errorf("sync: github adapter mirrors tickets (level task) only, got %q for %s — build the model with BuildTicketModel", n.Level, n.LocalID)
		}
		fields := map[string]string{"title": n.Title}
		for k, v := range map[string]string{"status": n.Status, "type": n.Type, "priority": n.Priority} {
			if v != "" {
				fields[k] = v
			}
		}
		action := "create"
		if id := n.Trackers[githubTracker]; id != "" {
			action = "update"
			if n.Terminal {
				action = "close"
			}
		}
		ops = append(ops, Op{Action: action, Level: "issue", LocalID: n.LocalID, Fields: fields})
	}
	return ops, nil
}

// FlatTickets is implemented by an Output that mirrors the flat ticket store rather
// than the five-level hierarchy.
type FlatTickets interface{ FlatTickets() }

// BuildModelFor builds the model out wants: the flat ticket model for a FlatTickets
// output, the hierarchy tree for everything else (Linear).
func BuildModelFor(out Output, mr hierarchy.ModelRollup, subtasks []hierarchy.Subtask) Model {
	if _, ok := out.(FlatTickets); ok {
		return BuildTicketModel(subtasks)
	}
	return BuildSyncModel(mr, subtasks)
}

// BuildTicketModel is the flat model: every store ticket as a parentless "task" node,
// in ascending numeric id order (so GitHub issue numbers follow ticket ids on a first
// run). It exists because BuildSyncModel drops every ticket without a resolvable
// parent, and a store that was never put through `hierarchy apply` has no parents — the
// live store is exactly that, and BuildSyncModel returns it as the lone project node.
func BuildTicketModel(subtasks []hierarchy.Subtask) Model {
	sorted := append([]hierarchy.Subtask(nil), subtasks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, aerr := strconv.Atoi(sorted[i].ID)
		b, berr := strconv.Atoi(sorted[j].ID)
		if aerr == nil && berr == nil {
			return a < b
		}
		if aerr != nil && berr != nil {
			return sorted[i].ID < sorted[j].ID
		}
		return aerr == nil // numeric ids before the rest
	})
	nodes := make([]Node, 0, len(sorted))
	for _, s := range sorted {
		nodes = append(nodes, Node{
			Level: "task", LocalID: s.ID, Title: unquoteScalar(s.Title), Status: s.Status,
			Terminal: terminalStatuses[normalizeStatus(s.Status)],
			Type:     s.Type, Priority: s.Priority, Trackers: s.Trackers,
		})
	}
	return Model{Nodes: nodes}
}

// unquoteScalar strips one level of YAML quoting from a frontmatter value. The
// frontmatter reader hands values back raw, so a quoted title would otherwise reach
// GitHub with its quotes.
func unquoteScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// TicketBody returns the text of ticket localID below its frontmatter — read only when
// the caller opted in to publishing bodies.
func TicketBody(storeRoot, issuesRoot, localID string) (string, error) {
	path := taskOrIssuePath(storeRoot, issuesRoot, localID)
	if path == "" {
		return "", fmt.Errorf("sync: no file for ticket %q", localID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sync: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", fmt.Errorf("sync: %s has no frontmatter", path)
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n")) + "\n", nil
		}
	}
	return "", fmt.Errorf("sync: %s frontmatter never closes", path)
}

// managedLabelPrefixes are the label namespaces leadkit owns on a mirrored issue.
var managedLabelPrefixes = []string{"type:", "priority:", "status:"}

func isManagedLabel(name string) bool {
	for _, p := range managedLabelPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// desiredLabels is the managed label set for a node. A value that is empty, or not a
// plain token, contributes no label rather than a mangled one.
func desiredLabels(n Node) []string {
	var out []string
	add := func(prefix, v string) {
		v = strings.TrimSpace(v)
		if v == "" || len(prefix)+len(v) > 50 || strings.ContainsAny(v, " \t,#\"'") {
			return
		}
		out = append(out, prefix+v)
	}
	add("type:", strings.ToLower(n.Type))
	add("priority:", strings.ToUpper(n.Priority))
	add("status:", normalizeStatus(n.Status))
	return out
}

// githubID formats trackers.github.
func githubID(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

// parseGithubID is githubID's inverse. ok is false for anything not in the
// "<owner>/<repo>#<number>" form — including a bare number, which names no repo.
func parseGithubID(id string) (repo string, number int, ok bool) {
	i := strings.LastIndex(id, "#")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil || n <= 0 || !strings.Contains(id[:i], "/") {
		return "", 0, false
	}
	return id[:i], n, true
}
