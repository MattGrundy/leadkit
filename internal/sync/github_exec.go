package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// WHY THIS EXECUTES IN PROCESS, with no agent half.
//
// Linear's work is split — leadkit plans, a sync-executor agent applies — because
// leadkit holds no Linear credential and only an agent session has the Linear MCP.
// That reason does not exist here: `gh` is installed and already authenticated, the
// house pattern is to shell out to it (internal/gh, internal/prcreate) rather than
// speak the API, and the credential never enters leadkit — it stays inside gh, so there
// is no token to put in a config file, a log line or a delegate's transcript. An agent
// half would add a hop, a prompt and a place for the plan to be reinterpreted, to
// solve a problem GitHub does not have.
//
// The plan/execute SPLIT is kept anyway, as Plan (offline, pure, shared with
// `sync plan`) and Execute (this file). The executor needs one thing the plan cannot
// have — the repo's current issues — for two reasons the local record cannot answer:
// an issue deleted remotely must be REPORTED, not re-created; and an issue created by a
// run that died before recording its number must be FOUND, not duplicated.

// GHRunner runs `gh <args>` with stdin (nil for none) and returns stdout. It is the one
// seam to the outside world, so tests never touch a real gh.
type GHRunner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// ExecGH is the real GHRunner. gh carries the credential; leadkit never sees it. Its
// stderr is included in errors (gh does not print tokens there).
func ExecGH(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("gh %s: %w: %s", args[0], err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("gh %s: %w", args[0], err)
	}
	return out, nil
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidRepo reports whether s is a plain "owner/name" — it is interpolated into an API
// path, so nothing else is accepted.
func ValidRepo(s string) bool { return repoRe.MatchString(s) && !strings.Contains(s, "..") }

// GitHubExecutor applies GitHubAdapter's ops to Repo through gh.
type GitHubExecutor struct {
	Repo string
	Run  GHRunner
	// Record writes a created or adopted issue's id into the ticket (RecordID). It is
	// called IMMEDIATELY after the issue is known, before anything else is done to it.
	Record func(output, localID, trackerID string) error
	// Body returns a ticket's text. nil is the default and means TITLE + METADATA ONLY:
	// no ticket body is read from disk, sent, or compared. Set it only on an explicit
	// opt-in — a published body cannot be unpublished.
	Body func(localID string) (string, error)
	// Apply false is a dry run: the remote is READ (one listing) and nothing is created,
	// changed or recorded.
	Apply bool
	// Pace is the wait after each write; GitHub throttles bursts of content creation.
	Pace  time.Duration
	Sleep func(time.Duration)
}

// requirePrivate refuses a mirror target that is not CONFIRMED private. The ticket titles are an index of
// unfixed defects, a published issue cannot be unpublished, and the project keeps a public repository
// beside the private one it develops in, so the refusal is made in code, by default, for a dry run as
// well as an apply (a rehearsal against a repo the real run would refuse promises nothing true). It
// asks the forge, not the profile: the repository's own visibility is the fact, and it cannot drift from
// a config entry. An answer that is not exactly "true", or none at all, is a refusal.
func (e *GitHubExecutor) requirePrivate(ctx context.Context) error {
	out, err := e.Run(ctx, nil, "api", "repos/"+e.Repo, "--jq", ".private")
	if err != nil {
		return fmt.Errorf("sync: refusing %s: cannot confirm it is private (%v). Issues are only ever mirrored into a repository the forge reports as private, because a published title cannot be unpublished", e.Repo, err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("sync: refusing %s: it is not private (the forge reported %q). Issues are only ever mirrored into a repository the forge reports as private, because a published title cannot be unpublished; there is no flag that overrides this", e.Repo, strings.TrimSpace(string(out)))
	}
	return nil
}

// Outcome actions. missing/foreign/mismatch are reports, not writes.
const (
	ActCreated   = "created"   // no issue existed: one was created and its id recorded
	ActAdopted   = "adopted"   // an issue carrying this ticket's marker existed unrecorded: its id was recorded, nothing created
	ActUpdated   = "updated"   // the recorded issue differed from the ticket and was patched
	ActUnchanged = "unchanged" // the recorded issue already matched
	ActMissing   = "missing"   // the recorded issue is gone from the repo: reported, NOT re-created
	ActForeign   = "foreign"   // the recorded id names another repo (or no repo): left alone
	ActMismatch  = "mismatch"  // the recorded issue's marker names a different ticket: left alone
)

// Outcome is what happened to one ticket.
type Outcome struct {
	LocalID string `json:"local_id"`
	Action  string `json:"action"`
	Number  int    `json:"number,omitempty"`
	Title   string `json:"title,omitempty"` // the issue title this ticket publishes — so a dry run shows exactly what would leave
	Detail  string `json:"detail,omitempty"`
}

// GitHubResult is a run's account. DryRun means no outcome was applied.
type GitHubResult struct {
	DryRun   bool      `json:"dry_run"`
	Outcomes []Outcome `json:"outcomes"`
}

// Count returns how many outcomes have action.
func (r GitHubResult) Count(action string) int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Action == action {
			n++
		}
	}
	return n
}

// Problems is the outcomes needing a human: missing, foreign and mismatch.
func (r GitHubResult) Problems() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Action == ActMissing || o.Action == ActForeign || o.Action == ActMismatch {
			out = append(out, o)
		}
	}
	return out
}

// FormatGitHubResult renders r, one line per ticket that is not unchanged, then totals.
func FormatGitHubResult(r GitHubResult) string {
	var b strings.Builder
	verb := map[string]string{ActCreated: "create", ActAdopted: "adopt", ActUpdated: "update"}
	for _, o := range r.Outcomes {
		if o.Action == ActUnchanged {
			continue
		}
		act := o.Action
		if r.DryRun {
			if v, ok := verb[o.Action]; ok {
				act = "would " + v
			}
		}
		fmt.Fprintf(&b, "%-12s %-6s", act, o.LocalID)
		if o.Number > 0 {
			fmt.Fprintf(&b, " #%d", o.Number)
		}
		if o.Title != "" {
			fmt.Fprintf(&b, " %q", o.Title)
		}
		if o.Detail != "" {
			fmt.Fprintf(&b, " %s", o.Detail)
		}
		b.WriteByte('\n')
	}
	if r.DryRun {
		fmt.Fprintf(&b, "\ndry run, nothing written: %d to create, %d to adopt, %d to update, %d unchanged, %d need attention\n",
			r.Count(ActCreated), r.Count(ActAdopted), r.Count(ActUpdated), r.Count(ActUnchanged), len(r.Problems()))
	} else {
		fmt.Fprintf(&b, "\n%d created, %d adopted, %d updated, %d unchanged, %d need attention\n",
			r.Count(ActCreated), r.Count(ActAdopted), r.Count(ActUpdated), r.Count(ActUnchanged), len(r.Problems()))
	}
	return b.String()
}

// remoteIssue is the subset of GitHub's issue object the mirror reads.
type remoteIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	StateReason string          `json:"state_reason"`
	Labels      []remoteLabel   `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type remoteLabel struct {
	Name string `json:"name"`
}

// parseIssueListing decodes `gh api --paginate --slurp`: an array of pages, each an
// array of issues. A bare array of issues (a single unslurped page) is accepted too.
// Pull requests are dropped from the issues returned, but still count towards max, the
// highest number seen: issues and PRs share one number space.
func parseIssueListing(data []byte) (issues []remoteIssue, max int, err error) {
	var pages [][]remoteIssue
	if err := json.Unmarshal(data, &pages); err != nil {
		var flat []remoteIssue
		if err2 := json.Unmarshal(data, &flat); err2 != nil {
			return nil, 0, fmt.Errorf("sync: parse issue listing: %w", err)
		}
		pages = [][]remoteIssue{flat}
	}
	for _, p := range pages {
		for _, is := range p {
			if is.Number > max {
				max = is.Number
			}
			if is.isPR() {
				continue // the issues endpoint returns PRs too; a PR is never a mirrored ticket
			}
			issues = append(issues, is)
		}
	}
	return issues, max, nil
}

func (r remoteIssue) isPR() bool {
	return r.PullRequest != nil && string(r.PullRequest) != "null"
}

// maxLagProbe bounds the forward read above — far more than a replica lags by.
const maxLagProbe = 50

// fetch reads one issue by number. found is false only when GitHub says the number does
// not exist (404, or 410 for a deleted issue); any other failure is an error, never
// "not found".
func (e *GitHubExecutor) fetch(ctx context.Context, number int) (remoteIssue, bool, error) {
	raw, err := e.Run(ctx, nil, "api", fmt.Sprintf("repos/%s/issues/%d", e.Repo, number))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 410") {
			return remoteIssue{}, false, nil
		}
		return remoteIssue{}, false, fmt.Errorf("sync: read issue #%d: %w", number, err)
	}
	var is remoteIssue
	if err := json.Unmarshal(raw, &is); err != nil || is.Number != number {
		return remoteIssue{}, false, fmt.Errorf("sync: read issue #%d: unreadable response", number)
	}
	return is, true, nil
}

func markerOf(body string) string {
	m := markerRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

func (e *GitHubExecutor) pace() {
	if e.Pace <= 0 {
		return
	}
	if e.Sleep != nil {
		e.Sleep(e.Pace)
		return
	}
	time.Sleep(e.Pace)
}

func (e *GitHubExecutor) api(ctx context.Context, method, path string, payload map[string]any) ([]byte, error) {
	args := []string{"api", "-X", method, path}
	var stdin []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		stdin = b
		args = append(args, "--input", "-")
	}
	return e.Run(ctx, stdin, args...)
}

// Execute applies ops (as GitHubAdapter.Plan emitted them, with their model) to the
// repo. In order: one read of every issue in the repo; then per ticket, in plan order:
//
//   - a recorded id -> must name THIS repo, must still exist (else missing: reported,
//     never re-created), must not carry another ticket's marker; then patched only where
//     it differs;
//   - no recorded id -> an issue carrying this ticket's marker is ADOPTED (its number is
//     recorded); otherwise one is created and its number recorded straight away, before
//     any further call touches it.
//
// A create whose outcome is unknown (the call errored, or the process died) is safe to
// repeat: the issue, if it was made, carries the marker, and the next run adopts it.
// For that reason a failed call stops the run rather than retrying it. Tickets that
// cannot be mirrored (missing/foreign/mismatch) are reported and the run continues. On
// error the result so far is returned with it.
func (e *GitHubExecutor) Execute(ctx context.Context, model Model, ops []Op) (GitHubResult, error) {
	res := GitHubResult{DryRun: !e.Apply}
	if !ValidRepo(e.Repo) {
		return res, fmt.Errorf("sync: %q is not an owner/name repo", e.Repo)
	}
	if err := e.requirePrivate(ctx); err != nil {
		return res, err
	}
	byLocal := make(map[string]Node, len(model.Nodes))
	for _, n := range model.Nodes {
		byLocal[n.LocalID] = n
	}

	raw, err := e.Run(ctx, nil, "api", "--paginate", "--slurp", "repos/"+e.Repo+"/issues?state=all&per_page=100")
	if err != nil {
		return res, fmt.Errorf("sync: list %s issues: %w", e.Repo, err)
	}
	issues, max, err := parseIssueListing(raw)
	if err != nil {
		return res, err
	}
	byNumber := make(map[int]remoteIssue, len(issues))
	byMarker := map[string]remoteIssue{}
	index := func(is remoteIssue) {
		byNumber[is.Number] = is
		if id := markerOf(is.Body); id != "" {
			if prev, dup := byMarker[id]; !dup || is.Number < prev.Number { // lowest number wins, deterministically
				byMarker[id] = is
			}
		}
	}
	for _, is := range issues {
		index(is)
	}

	// THE LISTING LAGS. GitHub's list endpoint serves from a replica that is seconds
	// behind: measured against a real repo, an issue created a moment earlier was
	// absent from the listing every time, while a GET of that issue by number found it
	// at once. An interrupted run is resumed right away, so trusting the listing alone
	// would not see the issue the dead run made and would create a second one. Issue and
	// PR numbers are one contiguous sequence, so whatever the lagging listing hides is
	// at max+1, max+2, ...: read forward until a number does not exist.
	for n := max + 1; ; n++ {
		if n > max+maxLagProbe {
			return res, fmt.Errorf("sync: %s has more than %d issues the listing does not show; refusing to guess", e.Repo, maxLagProbe)
		}
		is, found, err := e.fetch(ctx, n)
		if err != nil {
			return res, err
		}
		if !found {
			break
		}
		if !is.isPR() {
			index(is)
		}
	}

	for _, op := range ops {
		n, ok := byLocal[op.LocalID]
		if !ok {
			return res, fmt.Errorf("sync: op for %s has no node in the model", op.LocalID)
		}
		add := func(o Outcome) { res.Outcomes = append(res.Outcomes, o) }

		if recorded := n.Trackers[githubTracker]; recorded != "" {
			repo, num, ok := parseGithubID(recorded)
			if !ok || !strings.EqualFold(repo, e.Repo) {
				add(Outcome{LocalID: n.LocalID, Action: ActForeign, Detail: fmt.Sprintf("trackers.github is %q, not an issue of %s", recorded, e.Repo)})
				continue
			}
			remote, exists := byNumber[num]
			if !exists {
				// Absent from the listing is not enough to call an issue deleted: the
				// listing lags. Only the issue's own address answering "gone" does.
				var err error
				var is remoteIssue
				is, exists, err = e.fetch(ctx, num)
				if err != nil {
					return res, err
				}
				if exists && is.isPR() {
					add(Outcome{LocalID: n.LocalID, Action: ActMismatch, Number: num, Detail: fmt.Sprintf("#%d is a pull request, not an issue", num)})
					continue
				}
				remote = is
			}
			if !exists {
				add(Outcome{LocalID: n.LocalID, Action: ActMissing, Number: num, Detail: fmt.Sprintf("recorded issue %s no longer exists in the repo; not re-created (clear trackers.github to create a new one)", recorded)})
				continue
			}
			if m := markerOf(remote.Body); m != "" && m != n.LocalID {
				add(Outcome{LocalID: n.LocalID, Action: ActMismatch, Number: num, Detail: fmt.Sprintf("issue #%d is marked as ticket %s", num, m)})
				continue
			}
			o, err := e.reconcile(ctx, n, remote, ActUpdated)
			add(o)
			if err != nil {
				return res, err
			}
			continue
		}

		if remote, found := byMarker[n.LocalID]; found {
			if e.Apply {
				if err := e.Record(githubTracker, n.LocalID, githubID(e.Repo, remote.Number)); err != nil {
					return res, fmt.Errorf("sync: ticket %s: issue #%d exists but recording its id failed (a re-run will adopt it): %w", n.LocalID, remote.Number, err)
				}
			}
			o, err := e.reconcile(ctx, n, remote, ActAdopted)
			add(o)
			if err != nil {
				return res, err
			}
			continue
		}

		o, err := e.create(ctx, n)
		add(o)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// wantedBody is the body a ticket's issue should carry, and whether the mirror manages
// it after creation (only when bodies are opted in).
func (e *GitHubExecutor) wantedBody(n Node) (string, error) {
	if e.Body == nil {
		return metadataBody(n.LocalID), nil
	}
	text, err := e.Body(n.LocalID)
	if err != nil {
		return "", fmt.Errorf("sync: ticket %s body: %w", n.LocalID, err)
	}
	return fullBody(n.LocalID, text), nil
}

func wantedTitle(n Node) string { return "[" + n.LocalID + "] " + n.Title }

func stateReason(n Node) string {
	switch normalizeStatus(n.Status) {
	case "dropped", "superseded":
		return "not_planned"
	}
	return "completed"
}

func (e *GitHubExecutor) create(ctx context.Context, n Node) (Outcome, error) {
	out := Outcome{LocalID: n.LocalID, Action: ActCreated, Title: wantedTitle(n)}
	if !e.Apply {
		return out, nil
	}
	body, err := e.wantedBody(n)
	if err != nil {
		return out, err
	}
	labels := desiredLabels(n)
	if labels == nil {
		labels = []string{}
	}
	resp, err := e.api(ctx, "POST", "repos/"+e.Repo+"/issues", map[string]any{
		"title": wantedTitle(n), "body": body, "labels": labels,
	})
	if err != nil {
		return out, fmt.Errorf("sync: create issue for ticket %s (outcome unknown; a re-run finds it if it was made): %w", n.LocalID, err)
	}
	var made struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(resp, &made); err != nil || made.Number <= 0 {
		return out, fmt.Errorf("sync: create issue for ticket %s: unreadable response (a re-run finds the issue if it was made)", n.LocalID)
	}
	out.Number = made.Number
	// Record before anything else: from here on the id is durable and a crash cannot
	// lead to a second create.
	if err := e.Record(githubTracker, n.LocalID, githubID(e.Repo, made.Number)); err != nil {
		return out, fmt.Errorf("sync: ticket %s: created issue #%d but recording its id failed (a re-run will adopt it): %w", n.LocalID, made.Number, err)
	}
	e.pace()
	if n.Terminal {
		if _, err := e.api(ctx, "PATCH", fmt.Sprintf("repos/%s/issues/%d", e.Repo, made.Number),
			map[string]any{"state": "closed", "state_reason": stateReason(n)}); err != nil {
			return out, fmt.Errorf("sync: ticket %s: created issue #%d but closing it failed (a re-run will close it): %w", n.LocalID, made.Number, err)
		}
		e.pace()
	}
	return out, nil
}

// reconcile patches remote toward n, and only where it differs: title, the managed
// labels (foreign labels are kept), open/closed state, and the body only when bodies
// are opted in. action is ActUpdated for a recorded issue (reported ActUnchanged when
// nothing differs) or ActAdopted for an adopted one (always reported adopted: recording
// its id was the work, whether or not it also needed a patch).
func (e *GitHubExecutor) reconcile(ctx context.Context, n Node, remote remoteIssue, action string) (Outcome, error) {
	out := Outcome{LocalID: n.LocalID, Action: action, Number: remote.Number, Title: wantedTitle(n)}
	patch := map[string]any{}
	var changes []string

	if t := wantedTitle(n); remote.Title != t {
		patch["title"] = t
		changes = append(changes, "title")
	}

	want := desiredLabels(n)
	have := []string{}
	var foreign []string
	for _, l := range remote.Labels {
		if isManagedLabel(l.Name) {
			have = append(have, l.Name)
		} else {
			foreign = append(foreign, l.Name)
		}
	}
	if !sameSet(want, have) {
		all := append(append([]string{}, foreign...), want...)
		patch["labels"] = all
		changes = append(changes, "labels")
	}

	wantState := "open"
	if n.Terminal {
		wantState = "closed"
	}
	if remote.State != wantState {
		patch["state"] = wantState
		if wantState == "closed" {
			patch["state_reason"] = stateReason(n)
		} else {
			patch["state_reason"] = "reopened"
		}
		changes = append(changes, "state")
	}

	if e.Body != nil {
		body, err := e.wantedBody(n)
		if err != nil {
			return out, err
		}
		if normalizeBody(remote.Body) != normalizeBody(body) {
			patch["body"] = body
			changes = append(changes, "body")
		}
	}

	if len(changes) == 0 {
		if action == ActUpdated {
			out.Action = ActUnchanged
		}
		return out, nil
	}
	out.Action = action
	if action == ActAdopted {
		out.Detail = "also differs: " + strings.Join(changes, ", ")
	} else {
		out.Detail = strings.Join(changes, ", ")
	}
	if !e.Apply {
		return out, nil
	}
	if _, err := e.api(ctx, "PATCH", fmt.Sprintf("repos/%s/issues/%d", e.Repo, remote.Number), patch); err != nil {
		return out, fmt.Errorf("sync: patch issue #%d for ticket %s: %w", remote.Number, n.LocalID, err)
	}
	e.pace()
	return out, nil
}

func normalizeBody(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, c := range m {
		if c != 0 {
			return false
		}
	}
	return true
}
