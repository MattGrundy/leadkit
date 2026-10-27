// Package milestone is the machine-readable milestone + DoD anchor and the idle-gate
// that reads it (docs/PORTABLE-OPERATING-KIT.md, docs/build-tasks/
// milestone-anchor-idle-gate.md).
//
// The drift this exists to catch: an autonomous lead LOST the milestone goal, worked
// tactical threads, and declared "rest/idle — nothing needs you" while the milestone
// DoD was demonstrably unmet. There was nothing machine-readable to re-anchor or gate
// against. This package adds a QUERYABLE artifact — the current milestone plus an
// ordered list of DoD conditions, each with a signal that evaluates to met / unmet /
// unverified — and two readers over it: `milestone status` (the per-tick re-anchor)
// and `milestone check` (the idle-gate).
//
// FOUNDING PRINCIPLE: the LEAD can be idle; the TEAM cannot. A lead resting WITH work
// in flight (delegated to a builder/gate/CI) is legitimate and expected. The gate
// reports on TEAM/MILESTONE state — open milestone tasks + unverified/unmet DoD
// signals — never on lead activity. It fires only on a false claim that the WORK is at
// rest while the milestone is unmet.
//
// All EXTERNAL signal sources (pr/ci) are INJECTED interfaces so evaluation is
// hermetic — tests never hit real gh/CI. The `tasks` signal reuses internal/hierarchy's
// frontmatter query; `gate` reuses internal/gate records. `manual` is the explicit
// escape for a signal whose automated check does not exist yet: it defaults to
// UNVERIFIED (not met), keeping the gate closed until the check is built or a human
// marks it — you cannot declare done what you cannot verify.
package milestone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Condition type vocabulary — a DoD condition's `type:` field.
const (
	TypeTasks  = "tasks"  // met when ZERO open (non-terminal) store tasks resolve to the milestone
	TypePR     = "pr"     // met when a named PR-ref set is all merged (optionally in a named `repo:`)
	TypeGate   = "gate"   // met when a recorded ready gate verdict exists for a sha/branch
	TypeCI     = "ci"     // met when a named check-run/pipeline is Succeeded
	TypeManual = "manual" // no automated signal yet — defaults to unverified (keeps the gate closed)
	// TypeScript runs a lead+user-authored command (Condition.Script — inline, or a path
	// to an in-repo executable) and maps its EXIT STATUS to met/unmet/unverified. It is
	// the PORTABLE gate automation that SUPERSEDES the never-wired TypeCI: a CI-green DoD
	// is now `type: script` running the project's own `gh pr checks`/argo query, so any
	// project defines its OWN DoD check without a leadkit-native forge integration.
	// SAFETY: this EXECUTES arbitrary shell from milestones.yaml at the user's privileges
	// — the same trust level as leadkit.yaml, a git hook, or a Makefile target (the
	// milestones.yaml + referenced scripts are lead-authored and user-agreed). The verdict
	// is the EXIT CODE ONLY (stdout is captured for diagnosis, NEVER treated as success),
	// every run is timeout-bounded, and the Resolvers.ScriptEnabled opt-in can hard-disable
	// it (a disabled condition reads unverified without ever calling the runner).
	TypeScript = "script"
)

// State is one condition's evaluated verdict. Unverified is NOT met — it keeps the gate
// closed exactly like Unmet, but reads differently in the report (can't-verify vs
// verified-not-done).
type State string

const (
	Met        State = "met"
	Unmet      State = "unmet"
	Unverified State = "unverified"
)

// Condition is one DoD entry. The ref fields are polymorphic by Type: PRs for pr, Sha/
// Branch for gate, Check for ci, Milestone (optional override) for tasks. Unused fields
// stay zero.
type Condition struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	PRs       []string `yaml:"prs,omitempty"`
	Sha       string   `yaml:"sha,omitempty"`
	Branch    string   `yaml:"branch,omitempty"`
	Check     string   `yaml:"check,omitempty"`
	Script    string   `yaml:"script,omitempty"`    // script: an inline command OR a path to an in-repo executable
	Milestone string   `yaml:"milestone,omitempty"` // tasks: override the artifact's current_milestone
	// Repo is an OPTIONAL forge repo spec (e.g. "owner/repo") naming the
	// repo a signal's refs live in. Consumed by `pr` today — the resolver passes it to
	// `gh pr view <n> -R <repo>` so a cross-repo PR resolves instead of being read
	// against the gate's cwd repo (a permanent over-block). Empty = today's cwd-repo
	// behavior (no regression). Parsed on gate/ci too for forward-compat, though only pr
	// consumes it now.
	Repo string `yaml:"repo,omitempty"`
}

// Artifact is the whole parsed milestones.yaml DoD anchor: the current milestone id and
// its ordered DoD conditions.
type Artifact struct {
	CurrentMilestone string      `yaml:"current_milestone"`
	DoD              []Condition `yaml:"dod"`
}

// Parse reads and parses path (the milestones_file DoD anchor). A MISSING file returns
// (Artifact{}, false, nil) — NOT an error: an absent artifact degrades cleanly to "no
// milestone DoD declared", never a crash and never a false GO, matching every other
// "optional feature, absence is normal" reader in this codebase. A present-but-malformed
// file IS an error.
func Parse(path string) (Artifact, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, err
	}
	var a Artifact
	if err := yaml.Unmarshal(data, &a); err != nil {
		return Artifact{}, true, fmt.Errorf("milestone: parse %s: %w", path, err)
	}
	return a, true, nil
}

// TaskCounter counts open (non-terminal) store tasks that roll up to a milestone/epic id
// through the hierarchy — the `tasks` signal. It also reports `known`: whether the id
// names a real node anywhere in the hierarchy, so a zero count can be told apart — a
// KNOWN id with all-terminal tasks is a genuine MET, an UNKNOWN (mis-keyed) id is
// UNVERIFIED, never a silent false-MET. The real implementation reuses
// internal/hierarchy's rollup query (see OpenTasksForMilestone); tests inject a fake.
type TaskCounter interface {
	OpenTaskCount(milestoneID string) (count int, known bool, err error)
}

// PRResolver reports whether a single PR ref is merged in the named repo (repo is a forge
// spec like "owner/repo"; "" = the cwd repo). The real implementation shells
// to the forge CLI (internal/prcreate's Runner seam); tests inject a fake and never hit
// real gh.
type PRResolver interface {
	Merged(ref, repo string) (State, error)
}

// CIResolver reports whether a named check-run/pipeline Succeeded.
type CIResolver interface {
	Succeeded(name string) (State, error)
}

// GateResolver reports whether a recorded ready gate verdict exists for a sha/branch —
// the real implementation reuses internal/gate records.
type GateResolver interface {
	Ready(ref string) (State, error)
}

// ScriptRunner runs a `type: script` DoD signal's command and returns its exit status.
// scriptOrPath is EITHER an inline command (→ `bash -c <cmd>`) OR a path to an existing
// executable file (→ exec the file); the caller (evalScript) resolves a relative path and
// hands over the classified form. workdir is the process cwd for the run (the project
// root). The run MUST honor the ctx deadline (the timeout bound) — on timeout it returns
// an err wrapping context.DeadlineExceeded. A clean run (any exit code) returns err==nil
// with exitCode set; a could-not-run (not found, non-executable) returns a non-timeout
// err. Tests inject a FAKE that returns canned exit codes and NEVER touches a real shell.
type ScriptRunner interface {
	Run(ctx context.Context, scriptOrPath, workdir string) (exitCode int, output string, err error)
}

// Resolvers bundles the injected signal sources. Any nil resolver means "this signal
// cannot be evaluated here" → the condition reads Unverified (which keeps the gate
// closed, never a false GO). This is exactly the Layer-3 "signals graduate as their
// check is built" story: an unwired resolver is unverified, not met.
type Resolvers struct {
	Tasks TaskCounter
	PR    PRResolver
	CI    CIResolver
	Gate  GateResolver

	// Script is the injected runner for the `type: script` signal; nil → the condition
	// reads Unverified ("no script runner wired"), never a false Met.
	Script ScriptRunner
	// ScriptEnabled is the milestone.enable_script_signals opt-in: when false, a
	// `type: script` condition reads Unverified ("script signals disabled") and Script is
	// NEVER called — the categorical hard-disable for a project that forbids exec.
	ScriptEnabled bool
	// ScriptTimeout bounds every script run (from milestone.script_timeout). A run that
	// exceeds it is killed and reads Unverified. Non-positive means no bound.
	ScriptTimeout time.Duration
	// ScriptWorkdir is the project root — the runner's cwd AND the base a relative script
	// path resolves against.
	ScriptWorkdir string
	// ScriptResolve resolves a (possibly relative) script path to an absolute filesystem
	// path — the CLI wires prof.Resolve. Nil falls back to joining ScriptWorkdir.
	ScriptResolve func(string) string
}

// ConditionResult is one condition's evaluated state plus a human detail line.
type ConditionResult struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	State  State  `json:"state"`
	Detail string `json:"detail"`
}

// Met reports whether this condition counts toward a GO.
func (c ConditionResult) Met() bool { return c.State == Met }

// Report is the evaluated milestone snapshot — what `status` renders and `check`
// decides on.
type Report struct {
	Declared           bool              `json:"declared"`
	CurrentMilestone   string            `json:"current_milestone"`
	Conditions         []ConditionResult `json:"conditions"`
	OpenTaskCount      int               `json:"open_task_count"`
	OpenTaskCountKnown bool              `json:"open_task_count_known"`
}

// Evaluate computes the Report from a parsed artifact and the injected resolvers.
// present is Parse's found flag; when false the Report is the "no milestone declared"
// degrade state (Declared=false) and nothing else is evaluated.
func Evaluate(a Artifact, present bool, r Resolvers) Report {
	if !present {
		return Report{Declared: false}
	}
	rep := Report{Declared: true, CurrentMilestone: a.CurrentMilestone}

	// The open-milestone-task count is surfaced independently of any `tasks` DoD
	// condition — the re-anchor always shows how far the TEAM is from a clean milestone.
	if r.Tasks != nil {
		if n, _, err := r.Tasks.OpenTaskCount(a.CurrentMilestone); err == nil {
			rep.OpenTaskCount = n
			rep.OpenTaskCountKnown = true
		}
	}

	for _, c := range a.DoD {
		rep.Conditions = append(rep.Conditions, evalCondition(c, a.CurrentMilestone, r))
	}
	return rep
}

func evalCondition(c Condition, currentMilestone string, r Resolvers) ConditionResult {
	res := ConditionResult{Name: c.Name, Type: c.Type}
	switch c.Type {
	case TypeTasks:
		ms := c.Milestone
		if ms == "" {
			ms = currentMilestone
		}
		if r.Tasks == nil {
			res.State, res.Detail = Unverified, "no task counter wired"
			return res
		}
		n, known, err := r.Tasks.OpenTaskCount(ms)
		if err != nil {
			res.State, res.Detail = Unverified, fmt.Sprintf("could not count tasks for %s: %v", ms, err)
			return res
		}
		switch {
		case n > 0:
			res.State, res.Detail = Unmet, fmt.Sprintf("%d open task(s) for %s", n, ms)
		case !known:
			// Zero tasks AND the id matches no known milestone/epic/issue → a mis-keyed
			// id, not a genuine done. BLOCK (unverified), never a silent false-MET.
			res.State, res.Detail = Unverified, fmt.Sprintf("no tasks and no milestone/epic '%s' found — is the id right?", ms)
		default:
			res.State, res.Detail = Met, fmt.Sprintf("0 open tasks for %s", ms)
		}
	case TypePR:
		res.State, res.Detail = evalPR(c.PRs, c.Repo, r.PR)
	case TypeGate:
		ref := c.Sha
		if ref == "" {
			ref = c.Branch
		}
		var resolve singleResolver
		if r.Gate != nil {
			resolve = r.Gate.Ready
		}
		res.State, res.Detail = evalSingle(ref, "gate", resolve)
	case TypeCI:
		var resolve singleResolver
		if r.CI != nil {
			resolve = r.CI.Succeeded
		}
		res.State, res.Detail = evalSingle(c.Check, "ci check", resolve)
	case TypeScript:
		res.State, res.Detail = evalScript(c, r)
	case TypeManual:
		res.State, res.Detail = Unverified, "manual — no automated signal yet (mark or build a check to clear)"
	default:
		res.State, res.Detail = Unverified, fmt.Sprintf("unknown condition type %q", c.Type)
	}
	return res
}

// evalPR resolves a PR-ref set: met only when EVERY ref is merged; any unresolved ref
// makes the whole set unverified; any resolved-not-merged makes it unmet. An empty set
// or a nil resolver is unverified (can't confirm).
func evalPR(refs []string, repo string, pr PRResolver) (State, string) {
	if pr == nil {
		return Unverified, "no PR resolver wired"
	}
	if len(refs) == 0 {
		return Unverified, "no PR refs listed"
	}
	var unmerged, unresolved []string
	for _, ref := range refs {
		st, err := pr.Merged(ref, repo)
		switch {
		case err != nil || st == Unverified:
			unresolved = append(unresolved, ref)
		case st == Unmet:
			unmerged = append(unmerged, ref)
		}
	}
	if len(unresolved) > 0 {
		// Name the repo the resolver looked in so a wrong `repo:` is diagnosable, not
		// silent (empty repo = the cwd repo, today's message).
		if repo != "" {
			return Unverified, "unresolved in " + repo + ": " + strings.Join(unresolved, ", ")
		}
		return Unverified, "unresolved PR(s): " + strings.Join(unresolved, ", ")
	}
	if len(unmerged) > 0 {
		return Unmet, "unmerged PR(s): " + strings.Join(unmerged, ", ")
	}
	return Met, fmt.Sprintf("all %d PR(s) merged", len(refs))
}

// singleResolver is the shared shape of the gate/ci one-ref resolvers, after nil-guard.
type singleResolver func(ref string) (State, error)

func evalSingle(ref, label string, resolve singleResolver) (State, string) {
	if resolve == nil {
		return Unverified, "no " + label + " resolver wired"
	}
	if ref == "" {
		return Unverified, "no " + label + " ref named"
	}
	st, err := resolve(ref)
	if err != nil {
		return Unverified, fmt.Sprintf("could not resolve %s %q: %v", label, ref, err)
	}
	switch st {
	case Met:
		return Met, fmt.Sprintf("%s %q ready/succeeded", label, ref)
	case Unmet:
		return Unmet, fmt.Sprintf("%s %q not ready/succeeded", label, ref)
	default:
		return Unverified, fmt.Sprintf("%s %q unverified", label, ref)
	}
}

// scriptOutputMax bounds how much captured combined output is kept in a condition's
// detail — enough to diagnose a broken check, not so much it floods the report.
const scriptOutputMax = 500

// evalScript runs a `type: script` condition and maps its EXIT STATUS to a state. The
// verdict is the EXIT CODE ONLY — captured stdout/stderr is folded into the detail for
// diagnosis but NEVER interpreted as success (a script that prints "done" and exits 1 is
// unmet). Order of the safety guards matters: the enable-flag hard-stop comes FIRST, so a
// disabled project never reaches the runner.
func evalScript(c Condition, r Resolvers) (State, string) {
	if !r.ScriptEnabled {
		return Unverified, "script signals disabled (milestone.enable_script_signals=false)"
	}
	if r.Script == nil {
		return Unverified, "no script runner wired"
	}
	if strings.TrimSpace(c.Script) == "" {
		return Unverified, "no script command/path given"
	}

	// Classify: a value that RESOLVES to an existing regular file is executed as that
	// file (pass the absolute path); anything else is an inline command (pass verbatim →
	// bash -c). The resolution mirrors the CLI's prof.Resolve (project-relative).
	arg := c.Script
	resolved := arg
	if r.ScriptResolve != nil {
		resolved = r.ScriptResolve(arg)
	} else if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(r.ScriptWorkdir, resolved)
	}
	if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() {
		arg = resolved
	}

	ctx := context.Background()
	if r.ScriptTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.ScriptTimeout)
		defer cancel()
	}
	exit, output, err := r.Script.Run(ctx, arg, r.ScriptWorkdir)
	return mapScriptResult(exit, output, err)
}

// mapScriptResult is the documented exit-status → state mapping, verdict = exit code
// only: 0 → met, 1 → unmet, any other exit / a run error / a timeout → unverified (keeps
// the gate CLOSED, never a false met). The detail always NAMES the code or error so a
// broken check is diagnosable rather than silently passing.
func mapScriptResult(exit int, output string, err error) (State, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Unverified, scriptDetail("script timed out", output)
	case err != nil:
		return Unverified, scriptDetail(fmt.Sprintf("script could not run: %v", err), output)
	case exit == 0:
		return Met, scriptDetail("script exited 0 (met)", output)
	case exit == 1:
		return Unmet, scriptDetail("script exited 1 (unmet)", output)
	default:
		return Unverified, scriptDetail(fmt.Sprintf("script exited %d (unverified — non-0/1 exit)", exit), output)
	}
}

// scriptDetail appends the (truncated) captured output to a verdict message so a broken
// check is diagnosable — the output is DIAGNOSIS ONLY, never the verdict.
func scriptDetail(msg, output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return msg
	}
	if len(output) > scriptOutputMax {
		output = output[:scriptOutputMax] + "…(truncated)"
	}
	return msg + ": " + output
}

// Blocked reports whether `milestone check` would BLOCK (i.e. NOT a GO). GO requires:
// the artifact is declared, EVERY condition is met, AND zero open milestone tasks. An
// absent artifact BLOCKS (it is not a false GO — you can't confirm done what isn't
// declared). Any unmet/unverified condition, or any open task, blocks.
func (rep Report) Blocked() bool {
	if !rep.Declared {
		return true
	}
	if rep.OpenTaskCountKnown && rep.OpenTaskCount > 0 {
		return true
	}
	for _, c := range rep.Conditions {
		if !c.Met() {
			return true
		}
	}
	return false
}

// Blockers returns the human-readable reasons the gate is blocking — the open-task
// count (when non-zero) followed by each unmet/unverified condition, named. Empty when
// the milestone is a clean GO. Used by both `check`'s report and the SendMessage
// backstop's warning.
func (rep Report) Blockers() []string {
	var out []string
	if !rep.Declared {
		return []string{"no milestone DoD declared — run `leadkit milestone init` (see `leadkit doctor`)"}
	}
	if rep.OpenTaskCountKnown && rep.OpenTaskCount > 0 {
		out = append(out, fmt.Sprintf("%d open milestone task(s)", rep.OpenTaskCount))
	}
	for _, c := range rep.Conditions {
		if !c.Met() {
			out = append(out, fmt.Sprintf("%s [%s] %s — %s", c.Name, c.Type, c.State, c.Detail))
		}
	}
	return out
}

// Summary is a one-line "milestone <m>: N open task(s) + K unverified/unmet signal(s)"
// used by the SendMessage backstop's warning. Empty when not blocked.
func (rep Report) Summary() string {
	if !rep.Blocked() {
		return ""
	}
	if !rep.Declared {
		return "no milestone DoD declared"
	}
	signals := 0
	for _, c := range rep.Conditions {
		if !c.Met() {
			signals++
		}
	}
	return fmt.Sprintf("milestone %s: %d open task(s) + %d unmet/unverified signal(s)", rep.CurrentMilestone, rep.OpenTaskCount, signals)
}
