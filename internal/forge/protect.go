package forge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"mgpp.dev/leadkit/internal/redact"
)

// Options are the extras a protection write may ask for beyond the standard; the zero value
// asks for the standard only.
type Options struct {
	// RequireChecks are status-check contexts that must pass before merge. GitHub only —
	// GitLab gates on pipeline success at the project level, which this does not touch,
	// so asking for it there is refused rather than silently ignored.
	RequireChecks []string
}

// Call is one forge write, fully resolved: what runs, and the body it sends.
type Call struct {
	Method string
	Path   string
	Body   []byte
	Argv   []string // the exact CLI invocation; Body goes on its stdin
}

// DecideInput is everything Decide needs — already resolved, no I/O. Decide mirrors the
// pure-Decision shape `pr create`/`pr merge` use, so every refusal branch is testable
// without a forge.
type DecideInput struct {
	OriginURL    string
	AuthKey      string
	HasAuthority bool
	Target       Target
	TargetErr    error // non-nil: the forge/repo could not be resolved
	Current      Status
	Opts         Options
}

// Decision is Decide's verdict. Allowed with a nil Call means there is nothing to do.
type Decision struct {
	Allowed bool
	Noop    bool
	Reason  string
	Call    *Call
	// Plan says, in human terms, what the branch has now and what the write would change.
	// Set whenever the current state was read, including on a refusal, so the operator
	// sees WHY a write was refused (and what it would have removed), not only that it was.
	Plan *Plan
}

// Decide applies the governance ladder, default-deny at every step:
//  1. no authority.<repo> entry matches the origin: refuse (never touch an ungoverned repo).
//  2. the forge/repo cannot be resolved: refuse.
//  3. the current state is Unenforceable: refuse (the write cannot take effect here).
//  4. the current state is Unknown: refuse (never write against state we could not read).
//  5. already Verified: nothing to do.
//  6. otherwise compose the write that makes the branch meet the standard, diff it
//     against what the branch has now, and refuse if it would REMOVE any protection.
//     There is no flag that overrides that: weakening a branch is done by hand.
func Decide(in DecideInput) Decision {
	refuse := func(f string, a ...any) Decision {
		return Decision{Reason: fmt.Sprintf(f, a...)}
	}
	if !in.HasAuthority {
		return refuse("no authority.<repo> policy matches origin %s; authority grants are the repo owner's to give: ask the repo owner whether forge protection changes should be allowed here (do not add an entry yourself)", redact.Origin(in.OriginURL))
	}
	if in.TargetErr != nil {
		return refuse("cannot resolve the forge for %s: %v", in.AuthKey, in.TargetErr)
	}
	b := in.Current.Branch
	switch in.Current.State {
	case StateUnenforceable:
		return refuse("%s: %s — a protection write cannot take effect here", b, in.Current.Detail)
	case StateUnknown:
		return refuse("%s: %s — refusing to write protection against state that could not be read", b, in.Current.Detail)
	case StateVerified:
		d := Decision{Allowed: true, Noop: true, Reason: fmt.Sprintf("%s: already protected (%s); nothing to change", b, in.Current.Detail)}
		if in.Current.ghRules != nil {
			d.Plan = &Plan{Current: in.Current.ghRules.describe()}
		}
		return d
	}
	c, err := compose(in.Target, in.Current, in.Opts)
	if err != nil {
		return refuse("%s: %v", b, err)
	}
	plan, err := planFor(in.Target, in.Current, c)
	if err != nil {
		return refuse("%s: %v", b, err)
	}
	if rm := plan.Removals(); len(rm) > 0 {
		var lines []string
		for _, r := range rm {
			lines = append(lines, r.Text)
		}
		return Decision{Plan: &plan, Reason: fmt.Sprintf(
			"%s: REFUSED — this write would REMOVE %s (%s). leadkit never weakens a branch protection; change those by hand on the forge if that is what you want",
			b, plural(len(rm), "existing protection"), strings.Join(lines, "; "))}
	}
	return Decision{Allowed: true, Call: &c, Plan: &plan, Reason: fmt.Sprintf("%s: %s; will %s %s", b, in.Current.Detail, c.Method, c.Path)}
}

func compose(t Target, cur Status, opts Options) (Call, error) {
	var c Call
	switch t.Kind {
	case KindGitLab:
		if len(opts.RequireChecks) > 0 {
			return c, fmt.Errorf("--require-check is GitHub-only; GitLab gates merges on pipeline success at the project level, which leadkit does not change")
		}
		if len(cur.gitlabUnfixable) > 0 {
			return c, fmt.Errorf("GitLab grants the union of every covering rule, and %s; writing the exact rule cannot close that. Edit or remove those rules on the forge", strings.Join(cur.gitlabUnfixable, "; "))
		}
		proj := "projects/" + t.repoPath() + "/protected_branches"
		if ex := cur.gitlabExact; ex != nil {
			// Replace each existing push level with "No one". The ids come from the read we
			// just did. NOT VERIFIED against a live project: allowed_to_push with _destroy
			// is the update shape as the GitLab docs describe it for Premium/Ultimate, and
			// Free may only support delete-and-recreate. A rejection is loud (non-zero
			// exit), never a silent half-write.
			push := []map[string]any{}
			for _, l := range ex.PushAccessLevels {
				push = append(push, map[string]any{"id": l.ID, "_destroy": true})
			}
			push = append(push, map[string]any{"access_level": glNoOne})
			c.Method, c.Path = "PATCH", proj+"/"+url.PathEscape(cur.Branch)
			c.Body = mustJSON(map[string]any{"allow_force_push": false, "allowed_to_push": push})
		} else {
			c.Method, c.Path = "POST", proj
			c.Body = mustJSON(map[string]any{
				"name":               cur.Branch,
				"push_access_level":  glNoOne,
				"merge_access_level": 40, // Maintainers may merge; nobody pushes directly
				"allow_force_push":   false,
			})
		}
	default:
		// PUT /protection REPLACES the rule. Start from what is there and overlay only
		// the standard; never send a fixed body.
		var now ghRules
		switch {
		case cur.ghRules != nil:
			now = *cur.ghRules
		case cur.State == StatePartial:
			return c, fmt.Errorf("the existing protection rule was not read, and a write would replace it; refusing")
		}
		c.Method = "PUT"
		c.Path = "repos/" + t.Repo + "/branches/" + url.PathEscape(cur.Branch) + "/protection"
		c.Body = mergeGH(now, opts.RequireChecks).encode()
	}
	c.Argv = apiArgv(t, c.Method, c.Path, true)
	return c, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // inputs are maps of primitives; unreachable
	}
	return b
}

// Apply runs a Call composed by Decide, the only place this package writes to a forge (its other
// calls are GETs; `pr create|edit|merge` write through internal/prcreate). It returns
// the CLI's own error text (stderr, else stdout, else the exec error) and does not retry: a
// failed write is reported, never repeated, and a nil return is not proof, so callers must
// read the branch back.
func Apply(r Runner, c Call) error {
	stdout, stderr, err := r.Run(c.Argv, c.Body)
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(stdout))
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s", msg)
}
