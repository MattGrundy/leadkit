package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
)

// push-gate is the Go replacement for ~/.claude/review-gate/pretooluse-hook.sh — the
// PreToolUse(Bash) hook that DENIES a `git push` publishing a commit that lacks a
// passing review-gate record. The commit judged is the one the command PUBLISHES (the
// refspec's source, resolved in the target repo), never the HEAD of the checkout the
// session happens to be in; only a bare push, which publishes the current
// branch, is judged by HEAD. A push whose commits cannot be named is refused.
// It is the TEXT layer. The enforcement layer for a raw `git push` is the git pre-push hook
// (`leadkit init` installs it; it runs gate.CheckPush after the shell has finished expanding,
// so a spelling of `push` the text cannot read still reaches it, as long as the hook itself is
// not switched off). This hook reads the command line, which cannot be done completely, so
// its judgement of a push is a diagnostic that fails fast with a reason. It also refuses the
// flags that switch the pre-push hook off (push_disable.go), but only where it can first read
// the command as git's: an unreadable spelling of the verb plus --no-verify passes both layers,
// and only forge-side branch protection closes that. `leadkit push` gates in-process and
// passes --no-verify itself, so the hook has nothing to add there.
//
// REUSE, not fork: it shares push-guard's parsePushCommands (recognizing `git push`,
// `git -C <path> push`, `cd <path> && git push` and extracting the target repo) so the
// two push hooks agree on what a push is and where it targets. It judges EVERY push the
// command carries (one verdict for the line, decided by its worst constituent), and it
// delegates each verdict to internal/gate.Gate.CheckWithOutcome (scope, BYPASS env+file,
// required-layers check — exactly what the shell delegated to `review-gate.sh check`).
//
// THE SHELL BUG THIS FIXES (2026-08-03): the shell resolved a `~`-prefixed repo path
// but never expanded the `~` before `cd "$repo"`, so an unexpanded `~` made the `cd`
// fail silently and the hook blocked with an empty diagnostic. push-gate expands `~`
// and $ENV in Go (resolveRepoPath) BEFORE handing the path to Check, so a `~`-path
// resolves correctly and never recurs the empty-diagnostic block.
//
// FAIL OPEN, like every other hook here: a non-Bash tool, a command with no push in it, or
// a nil Gate all ALLOW. A push it genuinely can't evaluate (bad repo path, out-of-scope
// remote, a bare push on an unresolvable HEAD) is ALLOWED by the gate's own fail-open
// posture. Two exceptions fail CLOSED, because judging by anything else would be judging
// nothing or the wrong commit: an IN-SCOPE push that names refs the hook cannot resolve to
// commits (see pushedCommits), and a command the shared parser cannot read -- a subshell
// runner, `bash -c`, `eval`, `$( … )` and kin hold a push it never saw (pushUnsure, see
// push_parse.go and pushGateUnsure). A command with no push is not that: `bash -c 'make'`
// stays allowed.
//
// A third exception, narrower still: an out-of-scope verdict is an exemption only if a
// profile was READ to reach it. A repo whose profile exists and was not accepted, or that is
// evidently a leadkit project (a .leadkit/ directory) with no profile found, is REFUSED
// instead (PushGateConfig.ProfileProblem); a repo with no sign of leadkit stays allowed.
// Without the third case a broken profile and a deliberate exemption read the same.

// PushGateConfig is push-gate's resolved inputs. The review-gate the push is judged
// against is resolved PER-TARGET-REPO via ResolveGate — NOT baked from the session-
// ambient profile — so a push to another project's clone enforces THAT clone's own gate
// policy (scope + records_dir), matching what `config show`/`doctor` resolve from that
// repo. This is the ambient-profile class fix: whether a push is gated is decided by the
// TARGET repo's governance, never the pushing session's. The guard stays PURE — it loads
// no profiles; cmd/leadkit injects ResolveGate (see resolveTargetGate). ResolveGate == nil,
// or a nil *gate.Gate returned from it, disables the check (fail open). Cwd is the no-path
// fallback repo (the hook process's own cwd, which IS the session's cwd) used when the
// command carries no `git -C`/`cd` target. Home expands a leading `~` (empty →
// os.Getenv("HOME")).
type PushGateConfig struct {
	ResolveGate func(repoPath string) *gate.Gate
	Cwd         string
	Home        string

	// Mode resolves push-gate's ENFORCEMENT posture for the TARGET repo (advisory |
	// enforce), from that repo's own governing profile — the same per-target resolution
	// ResolveGate uses. Optional: nil (every existing test, an unconfigured adopter) means
	// ModeAdvisory. It flips ONLY the fail-open on the UNVERIFIABLE outcome
	// (gate.OutcomeUnverifiable — a bad repo path, an unresolvable HEAD, or an unresolvable
	// target origin): advisory allows it (today), enforce DENIES it (can't confirm the sha
	// is gated → block). A clear gated allow, a legitimate out-of-scope allow, and a clear
	// ungated deny are IDENTICAL in both modes. See docs/build-tasks/
	// configurable-guard-enforcement.md.
	Mode func(repoPath string) GuardMode

	// ProfileProblem says why the profile that governs repoPath could not be READ, or "" when
	// it was read or the repo is simply not a leadkit project. It is consulted ONLY for a
	// push the gate would otherwise allow as out of scope, because that verdict means "a
	// profile was read and does not govern this repo" only when a profile was read: a
	// profile that exists and was not accepted, or a repo that is evidently a leadkit project
	// (a .leadkit/ directory) with no profile found, would otherwise read as exempt, with the
	// built-in defaults standing in. A non-empty answer REFUSES (bypass aside), in both modes.
	// A repo with no sign of leadkit at all answers "" and stays allowed: this hook fires on
	// every push on the machine, and refusing there would be unusable. Optional: nil means every
	// profile reads fine (the one production wiring is cmd/leadkit/resolve.go). Ticket 204.
	ProfileProblem func(repoPath string) string

	// Unconfigured says whether repoPath is in the UNCONFIGURED state (ticket 302): it carries a
	// .leadkit/ runtime directory, and no profile exists for it at all, neither in the repo nor
	// in the external config. That is not ProfileProblem's case: nothing was turned away, there
	// is simply no policy, so "is this repo gated" has no answer to be unreadable. It is
	// consulted ONLY for a push the gate would otherwise allow as out of scope, and only after
	// ProfileProblem has said "" (a profile that exists and was not accepted always refuses).
	// A non-empty answer is the text of the state, and the push is ALLOWED WITH THAT TEXT
	// REPORTED on stderr: never a silent allow, because a gate that does not know whether it
	// governs a repo has not decided the repo is exempt. "" means the state does not hold.
	// Optional: nil (every existing test) never reports the state. See pushGateUnconfiguredNote.
	Unconfigured func(repoPath string) string
}

// reviewGateBypassVar is the env var whose presence bypasses the review-gate (the same
// name gate.CheckWithOutcome reads from the process env). The push-gate deny message
// advertises the INLINE form `REVIEW_GATE_BYPASS="reason" git push …`; PushGate honors it
// by parsing the leading assignment off the Bash command (see the inline-bypass allow
// below), because the hook runs in its OWN env BEFORE the pushed command, so it never sees
// an inline assignment the way the pushed subprocess does.
const reviewGateBypassVar = "REVIEW_GATE_BYPASS"

// PushGate runs the review-gate check for a `git push`, denying an ungated in-scope
// push and failing open on everything else.
func PushGate(in Input, cfg PushGateConfig) Result {
	if in.ToolName != "Bash" {
		return allow
	}
	// What the text layer is for: refuse the bounded set of ways to switch the pre-push hook off.
	if why, repos, first := hookDisabling(in.ToolInput.Command); why != "" {
		if res := pushGateDisable(in.ToolInput.Command, why, repos, first, cfg); res != allow {
			return res
		}
	}
	parsed := parsePushCommands(in.ToolInput.Command)
	if parsed.State == pushUnsure {
		// A command the parser cannot read may carry a push: refuse, never read it as "no
		// push". Before the leadkit-push allowance below, which must not whitewash it.
		return pushGateUnsure(parsed, cfg)
	}
	invocations, isPush := parsed.Invocations, parsed.State == pushFound
	// Our OWN gated primitive: a standalone `leadkit push …` gates in-process on the
	// resolved full sha, so re-checking it here would double-block it (and the internal
	// `git … push` it runs is a subprocess the hook never sees). Recognize + allow it —
	// but GUARDED by !isPush, so a command that ALSO carries a real `git push` (e.g.
	// `git push release/x && leadkit push …`) is still judged on that raw push; the leadkit
	// invocation can never whitewash a co-present bare push.
	if commandIsLeadkitPush(in.ToolInput.Command) && !isPush {
		return allow
	}
	if !isPush {
		return allow // not a git push — this guard doesn't govern it
	}
	if cfg.ResolveGate == nil {
		return allow // no per-target resolver wired — fail open
	}
	// INLINE bypass reachability: an inline `REVIEW_GATE_BYPASS="reason" git push …` sets
	// the var for the PUSHED subprocess, but this hook runs in its own env beforehand, so
	// gate.CheckWithOutcome's os.Getenv would never see it — the documented escape hatch
	// would be unreachable for the hook path. Parse the leading assignments off the command
	// and honor an inline REVIEW_GATE_BYPASS as an intentional bypass. It prefixes ONE
	// command, so it covers only the push that opens the line, never a later one.
	v, ok := leadingAssignments(in.ToolInput.Command)[reviewGateBypassVar]
	inlineBypass := ok && v != ""

	// One verdict for the whole line, decided by its worst constituent: every push is
	// judged, and the first that is refused refuses the command.
	var notes unconfiguredNotes
	for _, inv := range invocations {
		if inlineBypass && inv.leading {
			continue
		}
		label := ""
		if len(invocations) > 1 {
			label = inv.String()
		}
		if res := pushGateOne(inv, label, cfg, &notes); res != allow {
			return res
		}
	}
	return notes.result()
}

// unconfiguredNotes collects the UNCONFIGURED state (PushGateConfig.Unconfigured) reported by
// the pushes of one command line, so that a line carrying several is judged on all of them
// (a later push may still be refused) and the state is reported once per repo, whatever the
// verdict on the line turns out to be.
type unconfiguredNotes []string

// add records the note for repo, once.
func (n *unconfiguredNotes) add(repo, note string) {
	if n == nil {
		return
	}
	line := UnconfiguredWarning(repo, note)
	for _, have := range *n {
		if have == line {
			return
		}
	}
	*n = append(*n, line)
}

// result is the allow that carries the collected states, or the plain allow when there are none.
//
// The report goes where it is read. On a PreToolUse hook that exits 0, stderr reaches neither the
// user nor the model; stdout JSON does: systemMessage is shown to the user, and
// hookSpecificOutput.additionalContext is given to the model. There is deliberately no
// permissionDecision, so the command goes through the ordinary permission flow and this hook
// overrides nothing. The text is also on stderr, which is where a debug log of the hook finds it.
func (n *unconfiguredNotes) result() Result {
	if n == nil || len(*n) == 0 {
		return allow
	}
	text := strings.Join(*n, "\n")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(unconfiguredPayload{
		SystemMessage:      text,
		HookSpecificOutput: unconfiguredOutput{HookEventName: "PreToolUse", AdditionalContext: text},
	})
	return Result{Stdout: buf.String(), Stderr: text + "\n", ExitCode: 0}
}

// unconfiguredPayload is the stdout JSON of an allow that reports the unconfigured state.
type unconfiguredPayload struct {
	SystemMessage      string             `json:"systemMessage"`
	HookSpecificOutput unconfiguredOutput `json:"hookSpecificOutput"`
}

type unconfiguredOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// UnconfiguredWarning is the report that accompanies an allow in the UNCONFIGURED state. It
// names the state, says what it means (the push was not judged against any gate, because there
// is none to judge it against), and says how to leave it. It is the whole of the difference
// between a decision and a silent permit. Exported so the git pre-push hook and `leadkit push`
// say exactly what the PreToolUse hook says.
func UnconfiguredWarning(repo, note string) string {
	return "Review-gate: WARNING, not a block — " + filepath.Base(repo) + " is UNCONFIGURED: " + note +
		" This push was ALLOWED WITHOUT a review-gate check, because no gate exists for this repo to check it against; the repo is not known to be exempt." +
		" Create a profile (`leadkit config init`, or a leadkit.yaml) to bring it under the gate, or remove the .leadkit/ directory if this is not a leadkit project." +
		" [push-gate state: unconfigured]"
}

// pushGateUnsure refuses a command the parser could not read. The push it may hide has no
// target the hook can name, so scope is decided by the session's repo and by every repo the
// command visibly moves into (`cd`, `-C`): the command is refused if any is gated and not
// bypassed, and allowed where the gate does not govern. The inline-bypass form is NOT
// honoured here — it prefixes one command and the hidden push is not known to be it; the
// env var and the records dir's BYPASS file are, via CheckUndetermined.
func pushGateUnsure(p pushParse, cfg PushGateConfig) Result {
	if cfg.ResolveGate == nil {
		return allow // no per-target resolver wired — fail open
	}
	if p.Unknown != "" {
		// A push, or a move into a directory, that the text does not name: its repo may be one
		// this gate governs, so only the bypass lets it through.
		return pushGateUnknownTarget(p.Unknown, cfg)
	}
	sessionRepo, _ := resolveRepoPath("", cfg.Cwd, cfg.Home)
	repos := []string{sessionRepo}
	for _, r := range p.Repos {
		resolved, known := resolveRepoPath(r, cfg.Cwd, cfg.Home)
		if !known {
			// A variable the hook does not hold: the same unknown target.
			return pushGateUnknownTarget("the command moves into `"+r+"`, which uses a variable the hook cannot read", cfg)
		}
		if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
			repos = append(repos, resolved)
		}
	}
	var notes unconfiguredNotes
	for _, repo := range repos {
		g := cfg.ResolveGate(repo)
		if g == nil {
			continue
		}
		mode := ModeAdvisory
		if cfg.Mode != nil {
			mode = cfg.Mode(repo)
		}
		ok, outcome, reason := g.CheckUndetermined(repo, p.Why)
		var res Result
		if outcome == gate.OutcomeUngated {
			res = jsonDeny(pushGateUnsureReason(p.Why, g.RecordsDir, mode))
		} else if r, refused := cfg.unreadableProfile(g, repo, outcome, mode, &notes); refused {
			res = r
		} else {
			res = pushGateVerdict(g, mode, ok, outcome, reason, true)
		}
		if res != allow {
			return res
		}
	}
	return notes.result()
}

// unreadableProfile turns an out-of-scope verdict into a refusal when the profile that
// should have decided the scope could not be read (PushGateConfig.ProfileProblem). refused
// is false for every other verdict, and for an out-of-scope one that is bypassed or whose
// profile read fine, so the caller goes on to the ordinary verdict.
//
// A repo that is UNCONFIGURED (PushGateConfig.Unconfigured) is the one exception that is not
// a refusal: it is recorded in notes so the allow that follows carries the state, and refused
// stays false. A profile problem is asked first and always wins.
func (cfg PushGateConfig) unreadableProfile(g *gate.Gate, repo string, outcome gate.CheckOutcome, mode GuardMode, notes *unconfiguredNotes) (res Result, refused bool) {
	if outcome != gate.OutcomeOutOfScope {
		return allow, false
	}
	why := ""
	if cfg.ProfileProblem != nil {
		why = cfg.ProfileProblem(repo)
	}
	if why == "" {
		if cfg.Unconfigured != nil {
			if note := cfg.Unconfigured(repo); note != "" {
				notes.add(repo, note)
			}
		}
		return allow, false
	}
	if _, oc, _ := g.CheckUnreadableProfile(why); oc != gate.OutcomeUngated {
		return allow, false // the BYPASS escape hatch is in force
	}
	return jsonDeny(pushGateProfileReason(repo, why, g.RecordsDir, mode)), true
}

// pushGateProfileReason is the deny for a push into a repo whose governing profile could
// not be read. It says the built-in default is not an exemption, and where to look.
func pushGateProfileReason(repo, why, recordsDir string, mode GuardMode) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "Review-gate: push blocked — whether " + filepath.Base(repo) + " is gated cannot be known, because " + why + "." +
		" The built-in default that stands in for an unreadable profile governs nothing, and that is an unknown, not a decision to exempt the repo (fail-closed)." +
		" Fix the environment (HOME, the working directory), repair or add the profile (a leadkit.yaml), or remove the .leadkit/ directory if this is not a leadkit project." +
		"  |  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + "." +
		" [push-gate mode: " + mode.String() + "]"
}

// pushGateUnsureReason is the deny for a command the parser could not read.
func pushGateUnsureReason(why, recordsDir string, mode GuardMode) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "Review-gate: command blocked — it may contain a `git push` the gate cannot read (" + why + "), so it cannot be judged against the commits it publishes. " +
		unreadablePushAdvice +
		"  |  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + "." +
		" [push-gate mode: " + mode.String() + "]"
}

// String renders the push as the user typed it, so a refusal of one push among several
// can name which.
func (inv pushInvocation) String() string {
	words := []string{"git"}
	if inv.repoPath != "" {
		words = append(words, "-C", inv.repoPath)
	}
	words = append(words, "push")
	return strings.Join(append(words, inv.args...), " ")
}

// pushGateOne judges ONE git-push invocation against the review-gate of the repo it
// targets. label, when non-empty, names the push in a refusal (set only for a line that
// carries several).
func pushGateOne(inv pushInvocation, label string, cfg PushGateConfig, notes *unconfiguredNotes) Result {
	repo, known := resolveRepoPath(inv.repoPath, cfg.Cwd, cfg.Home)
	if !known {
		return pushGateUnknownTarget("the repo path `"+inv.repoPath+"` uses a variable the hook cannot read (a variable set in an earlier Bash call is not visible to the hook: spell the path out)", cfg)
	}
	if inv.repoPath != "" && inv.afterOthers {
		// Something earlier on the line may have made this directory (`git clone u d && cd d &&
		// …`). A path that does not exist now is then not a push that cannot run.
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			return pushGateUnknownTarget("the repo path `"+inv.repoPath+"` does not exist yet, and a command earlier on the line may create it", cfg)
		}
	}
	name := func(reason string) string {
		if label == "" {
			return reason
		}
		return "`" + label + "`: " + reason
	}

	// Resolve the review-gate that GOVERNS this target repo (its own project's scope +
	// records_dir, or the built-in default for an unprofiled repo — never the session's).
	g := cfg.ResolveGate(repo)
	if g == nil {
		return allow // resolver declined to produce a gate — fail open
	}

	mode := ModeAdvisory
	if cfg.Mode != nil {
		mode = cfg.Mode(repo)
	}

	// Judge the commits the push PUBLISHES, not whatever the repo's HEAD happens to be: the
	// gate is keyed by commit, so the question is which commit(s) the command names. This is
	// the same (full sha, target repo) judgement `leadkit push` makes — g.CheckWithOutcome on
	// an explicit full sha — and the hook must not substitute the session's own HEAD for it
	// (that would let an ungated push through from a gated checkout, and block a gated push
	// from an ungated one).
	// A verdict of "out of scope" only means the repo is exempt if a profile was READ: see
	// PushGateConfig.ProfileProblem. Every verdict below goes through here.
	verdict := func(ok bool, outcome gate.CheckOutcome, reason string, undetermined bool) Result {
		if res, refused := cfg.unreadableProfile(g, repo, outcome, mode, notes); refused {
			return res
		}
		return pushGateVerdict(g, mode, ok, outcome, reason, undetermined)
	}
	pushed := pushedCommits(g, repo, inv.args, inv.gitConfig)
	if pushed.undetermined != "" {
		// Cannot name what is being published: refuse (modulo scope and bypass), never
		// fall back to the ambient HEAD.
		ok, outcome, reason := g.CheckUndetermined(repo, pushed.undetermined)
		return verdict(ok, outcome, name(reason), true)
	}
	if pushed.head {
		// A bare push publishes the repo's current branch, so its HEAD IS the pushed ref.
		// sha="" lets CheckWithOutcome resolve it, keeping the unresolvable-HEAD posture
		// (advisory allows, enforce denies) — git itself fails to push an unborn branch.
		ok, outcome, reason := g.CheckWithOutcome("", repo)
		if res := verdict(ok, outcome, name(reason), false); res != allow {
			return res
		}
	}
	for _, c := range pushed.commits {
		ok, outcome, reason := g.CheckWithOutcome(c.sha, repo)
		if !ok || outcome == gate.OutcomeUngated {
			reason = c.ref + " (" + c.sha[:12] + "): " + reason
		}
		if res := verdict(ok, outcome, name(reason), false); res != allow {
			return res
		}
	}
	return allow // every published commit is gated, or the push publishes none (deletes only)
}

// pushGateVerdict turns one gate verdict into the hook's Result. A clear gated PASS and a
// legitimate out-of-scope repo ALLOW in both modes; a clear in-scope-but-ungated push
// DENIES in both. Only the UNVERIFIABLE outcome (bad repo path / unresolvable HEAD /
// unresolvable scope) differs: advisory allows, enforce denies (fail-closed).
// undetermined marks a verdict from CheckUndetermined, whose deny names the real problem
// instead of advising how to record layers for a sha that was never found.
func pushGateVerdict(g *gate.Gate, mode GuardMode, ok bool, outcome gate.CheckOutcome, reason string, undetermined bool) Result {
	switch outcome {
	case gate.OutcomeUnverifiable:
		if mode.Enforcing() {
			if gate.OriginUnreadable(reason) {
				return jsonDeny(pushGateOriginUnreadableReason(reason, g.RecordsDir, mode))
			}
			return jsonDeny(pushGateUnverifiableReason(reason, mode))
		}
		return allow // advisory fail-open
	case gate.OutcomeUngated:
		if undetermined {
			return jsonDeny(pushGateUndeterminedReason(reason, g.RecordsDir, mode))
		}
		return jsonDeny(pushGateDenyReason(reason, g.RecordsDir, mode))
	default: // OutcomeGated, OutcomeOutOfScope — allow in both modes
		if ok {
			return allow
		}
		// Defensive: any not-ok that isn't classified above still denies (never a silent
		// allow of a real ungated push).
		return jsonDeny(pushGateDenyReason(reason, g.RecordsDir, mode))
	}
}

// pushedCommit is one commit a push publishes: the ref as the command spelled it and the
// full sha it resolves to in the target repo.
type pushedCommit struct {
	ref string
	sha string
}

// pushedSet is what a push publishes. head means a bare push (no refspec), which publishes
// the repo's current branch. undetermined, when non-empty, says why the published commits
// could not be named.
type pushedSet struct {
	commits      []pushedCommit
	head         bool
	undetermined string
}

// pushedCommits resolves the commits a push's arguments (everything after "push")
// publish, in the TARGET repo, through the gate's own strict resolver (gate.ResolveCommit:
// a full commit sha or nothing). It never reads the session's HEAD for a push that names a
// ref. A push whose commits cannot be named — an unresolvable ref, a wildcard refspec,
// --all/--mirror/--tags/--follow-tags — comes back undetermined. A deletion publishes no commit and
// contributes none.
func pushedCommits(g *gate.Gate, repo string, args []string, gitConfig []string) pushedSet {
	var out pushedSet
	deleting := false
	for _, a := range args {
		if o := pushRefSetOption(a); o != "" {
			out.undetermined = "--" + o + " (as written: " + a + ") publishes a set of refs the command does not name"
			return out
		}
		switch a {
		case "--delete", "-d":
			deleting = true
		}
	}
	remote, refspecs, _ := parsePushArgs(args)
	if why := mirroredRemote(repo, remote, gitConfig); why != "" {
		out.undetermined = why
		return out
	}
	if why := followTagsConfigured(repo, args, gitConfig); why != "" {
		out.undetermined = why
		return out
	}
	if len(refspecs) == 0 {
		if why := unnamedRefSetConfig(repo, remote, gitConfig); why != "" {
			out.undetermined = why
			return out
		}
		out.head = true
		return out
	}
	seen := map[string]bool{}
	for _, r := range refspecs {
		r = strings.TrimPrefix(r, "+")
		if r == ":" {
			// Git's spelling of "push every matching branch": it names no ref.
			out.undetermined = "the refspec `:` pushes every branch that exists on both sides"
			return out
		}
		src := r
		if i := strings.Index(r, ":"); i >= 0 {
			src = r[:i]
			if src == "" { // ":<dst>" deletes dst
				continue
			}
		} else if deleting {
			continue
		}
		sha, ok := g.ResolveCommit(repo, src)
		if !ok {
			out.undetermined = "ref " + strconv.Quote(src) + " does not resolve to a commit in " + repo
			return out
		}
		if !seen[sha] {
			seen[sha] = true
			out.commits = append(out.commits, pushedCommit{ref: src, sha: sha})
		}
	}
	return out
}

// resolveRepoPath resolves the target repo path with proper ~/$ENV expansion IN GO —
// the exact step the shell botched. An empty repoPath (no `git -C`/`cd` target) falls
// back to cwd, mirroring push-guard's own repoPath=="" -> process-cwd convention. A
// leading `~` (or bare `~`) expands to home; env vars expand via the hook's environment.
// The result is handed to gate.Check, whose os.Stat + fail-open posture handles a path
// that still doesn't resolve to a real dir (allow) — never the silent-cd-fail block the
// shell produced.
//
// known is false when the path cannot be known from the command and the hook's
// environment: it names a variable the hook does not hold (or holds empty), which the
// command itself may have set (`R=/repo; cd $R && …`). Expansion then yields a path that
// names some other directory, or none. Variables the line assigns, globs, `~user` and the
// like are reported unknown by the parser before they get here (pathUnknownWhy). A plain path
// that does not exist is known: a `cd` or `git -C` to it fails at check time and no push runs
// then (a directory created between the check and the cd is a residual the hook cannot see), unless a
// command earlier on the line may create it (pushInvocation.afterOthers, checked by the caller).
func resolveRepoPath(repoPath, cwd, home string) (path string, known bool) {
	p := repoPath
	if p == "" {
		p = cwd
	}
	known = true
	p = os.Expand(p, func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			known = false
		}
		return v
	})
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home != "" {
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[len("~/"):])
		}
	}
	return p, known
}

// pushGateUnknownTarget refuses a command that moves into, or pushes from, a repo the hook
// cannot name, unless the BYPASS escape hatch is in force. Scope is no help: it is a
// property of the repo, and the repo is the unknown. why says what could not be named. The
// only gate that can be consulted is the session's own, for its BYPASS file.
func pushGateUnknownTarget(why string, cfg PushGateConfig) Result {
	if cfg.ResolveGate == nil {
		return allow // no per-target resolver wired — fail open
	}
	sessionRepo, _ := resolveRepoPath("", cfg.Cwd, cfg.Home)
	g := cfg.ResolveGate(sessionRepo)
	if g == nil {
		return allow
	}
	mode := ModeAdvisory
	if cfg.Mode != nil {
		mode = cfg.Mode(sessionRepo)
	}
	ok, outcome, reason := g.CheckUnknownTarget(why)
	if outcome == gate.OutcomeUngated {
		return jsonDeny(pushGateUnsureReason(why, g.RecordsDir, mode))
	}
	return pushGateVerdict(g, mode, ok, outcome, reason, true)
}

// pushGateDenyReason mirrors the shell's deny message: the gate's own reason, how to
// record the missing layers, and the BYPASS override note (env + the records_dir/BYPASS file).
func pushGateDenyReason(reason, recordsDir string, mode GuardMode) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "Review-gate: push blocked. " + reason +
		"  Record layers with: `leadkit gate record --layer <code-review|critique> --verdict ready --mutants \"<mutant -> failing test>\"` (or `--no-mutant \"<why>\"`)." +
		"  |  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + "." +
		" [push-gate mode: " + mode.String() + "]"
}

// pushGateUndeterminedReason is the deny for a push whose published commits could not be
// named. Unlike pushGateDenyReason it does not advise recording layers — there is no sha
// to record against — it says to name the refs, or to use the governed verb.
func pushGateUndeterminedReason(reason, recordsDir string, mode GuardMode) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "Review-gate: push blocked — " + reason + ", so it cannot be judged against the commits it publishes." +
		"  Name the branch to push (`<remote> <branch>`) or use `leadkit push --repo <clone> --branch <branch>`." +
		"  |  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + "." +
		" [push-gate mode: " + mode.String() + "]"
}

// pushGateOriginUnreadableReason is the deny for a push into a repo whose profile governs it but whose
// `origin` cannot be read. The cause is the remote's name, so it says that and how to fix it; the generic
// unverifiable advice ("push from the real repo with a resolvable HEAD") would send the user elsewhere.
// Every way out is named, the BYPASS escape hatch included: it lifts this refusal in enforce mode.
func pushGateOriginUnreadableReason(reason, recordsDir string, mode GuardMode) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "Review-gate: push blocked — " + strings.TrimSuffix(reason, " — allow") + ". This repo's guards are ENFORCING, so a push the gate cannot confirm is gated is denied (fail-closed)." +
		"  Fix: " + gate.OriginUnreadableFix + "." +
		"  |  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + ", or set enforce→advisory for this repo." +
		" [push-gate mode: " + mode.String() + "]"
}

// pushGateUnverifiableReason is the enforce-mode fail-CLOSED deny for a push the gate
// could not evaluate (bad repo path / unresolvable HEAD / unresolvable scope) — advisory
// would have allowed this. A push whose gated-ness can't be confirmed is treated as
// ungated. Names the mode so the authoritative posture is visible.
func pushGateUnverifiableReason(reason string, mode GuardMode) string {
	return "Review-gate: push blocked — could not confirm this push is gated (" + strings.TrimSuffix(reason, " — allow") +
		"), and this repo's guards are ENFORCING, so an unverifiable push is denied (fail-closed)." +
		"  Push from the real repo with a resolvable HEAD, or set enforce→advisory for this repo to restore fail-open." +
		" [push-gate mode: " + mode.String() + "]"
}
