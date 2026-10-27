package hook

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
)

// The text layer's job is bounded to what disables enforcement (ticket 171). A raw `git push`
// is gated by the git pre-push hook, which git runs after the shell has finished expanding, so
// the set of spellings of `push` is not what enforcement rests on. What it rests on is whether
// that hook still runs, and git has a short, closed list of ways to stop it:
//
//   - `--no-verify` on the push (and the unambiguous abbreviations git accepts of it);
//   - a `core.hooksPath` assignment, which sends git to another hooks directory:
//     `-c core.hooksPath=…`, `--config-env=core.hooksPath=…`, `GIT_CONFIG_PARAMETERS` /
//     `GIT_CONFIG_KEY_<n>` naming it, or `git config core.hooksPath …`.
//
// Those are refused here, by name, where the gate governs the repo. `-n` is NOT on the list:
// on `git push` it is `--dry-run` (git-push(1)), not a way to skip a hook.
//
// The FLAGS are a closed list; recognising the push they belong to is not. The `--no-verify`
// rule applies only when the subcommand is a literal `push` (or a command-line alias for it),
// so `git $'push' --no-verify`, `git ${IFS}push --no-verify` and a `core.hooksPath` key built
// by expansion (`core.$K`) pass this layer and, since the flag skips the hook, the hook too.
// No more spellings are added here: that is the unbounded problem the hook exists to leave.
// The combination cannot be closed on the client, because a local hook the local command can
// switch off is a control against accident; forge-side branch protection is what closes it.
//
// Also not covered: deleting, chmod-ing or editing the hook file (the command names the path,
// and it is not refused), an alias defined in a gitconfig file that adds --no-verify, a literal
// `git config core.hooksPath <value>` inside a command substitution, backticks, a process
// substitution or a heredoc body (it is refused inside `( )`, `{ }`, `bash -c` and `bash <<<`), config
// files redirected with GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM/HOME/XDG_CONFIG_HOME, and a clone
// that never installed the hook, which `leadkit doctor` reports.

// isNoVerify reports whether w is --no-verify or an abbreviation of it git would accept
// (parse-options takes any unambiguous prefix of a long option; every prefix of --no-verify
// from --no-v is treated as the flag, because refusing one that git would read as something
// else costs a retry, and missing one that it reads as this switches the hook off).
func isNoVerify(w string) bool {
	const full = "--no-verify"
	w = strings.ToLower(w)
	if strings.HasPrefix(w, full+"=") {
		return true
	}
	return len(w) >= len("--no-v") && strings.HasPrefix(full, w)
}

const hooksPathKey = "core.hookspath"

func mentionsHooksPath(s string) bool { return strings.Contains(strings.ToLower(s), hooksPathKey) }

// readOnlyConfigFlags make `git config core.hooksPath` a read or an unset, which cannot
// redirect anything. Only a flag git treats as exclusive of a write belongs here: `--show-scope`
// is ignored on a write (`git config --show-scope core.hooksPath /x` sets it), and
// `--list`, `-l` and `--show-origin` are refused by some versions and ignored by others, so they
// are not trusted either. A read with the key alone is allowed by its operand count, with or
// without those flags.
var readOnlyConfigFlags = map[string]bool{
	"--get": true, "--get-all": true, "--get-regexp": true, "--unset": true, "--unset-all": true,
}

// globalValueOptions are git's global options that take a separate value.
var globalValueOptions = map[string]bool{
	"--git-dir": true, "--work-tree": true, "--namespace": true, "--super-prefix": true,
	"--exec-path": false, "--config-env": true, "--attr-source": true,
}

// hookDisabling is what the text of a Bash command shows about switching the pre-push hook
// off: why is empty when it shows nothing. repos are the directories the command names with
// `-C` or `cd`, besides the session's own, so the refusal can be scoped to the repos it
// touches. first is true when the offending segment opens the line (the only place an inline
// REVIEW_GATE_BYPASS assignment reaches).
func hookDisabling(cmd string) (why string, repos []string, first bool) {
	segs, _, _ := splitSegments(cmd)
	for si, sg := range segs {
		if name, args, ok := dirChange(sg.words); ok && name == "cd" {
			if t, w := cdTarget(append([]string{"cd"}, args...)); w == "" && t != "" {
				repos = append(repos, t)
			}
		}
		if w, rs := segmentDisables(sg); w != "" {
			return w, append(repos, rs...), si == 0
		}
	}
	return "", nil, false
}

// segmentDisables reads ONE simple command.
func segmentDisables(sg segment) (why string, repos []string) {
	// An assignment anywhere in the segment (a leading one, or an operand of env/export).
	for _, w := range sg.words {
		if !looksLikeAssignmentWord(w) {
			continue
		}
		name, val, _ := strings.Cut(w, "=")
		if strings.HasPrefix(name, "GIT_CONFIG_") && mentionsHooksPath(val) {
			return "`" + name + "` sets core.hooksPath, which sends git to another hooks directory", repos
		}
	}
	ci := commandStart(sg.words)
	// Transparent wrappers (`env A=b git …`, `command git …`) run their operand as the command.
	for ci < len(sg.words) {
		w := path.Base(sg.words[ci])
		if w != "env" && w != "command" && w != "exec" && w != "nohup" {
			break
		}
		wrapper := ci
		ci++
		for w == "env" && ci < len(sg.words) && looksLikeAssignmentWord(sg.words[ci]) {
			ci++
		}
		if ci < len(sg.words) && strings.HasPrefix(sg.words[ci], "-") {
			ci = wrapper // a wrapper option the guard does not read: the wrapper is the command
			break
		}
	}
	if ci >= len(sg.words) {
		return "", nil
	}
	name := path.Base(sg.words[ci])
	if name != "git" {
		if shellWrappers[name] {
			// A script, eval, xargs, sudo ... carries a command the text hides in one argument.
			all := append(append([]string{}, sg.words[ci+1:]...), sg.herestr...)
			joined := strings.Join(all, " ")
			if mentionsHooksPath(joined) {
				return "it passes core.hooksPath to a command (`" + name + "`) the guard cannot read into", nil
			}
			if pushToken.MatchString(joined) {
				for _, w := range strings.Fields(joined) {
					if isNoVerify(strings.Trim(w, `'"`)) {
						return "it passes --no-verify with a push to a command (`" + name + "`) the guard cannot read into", nil
					}
				}
			}
		}
		return "", nil
	}

	rest := sg.words[ci+1:]
	aliases := map[string][]string{}
	i := 0
	for i < len(rest) {
		w := rest[i]
		switch {
		case w == "-C" && i+1 < len(rest):
			repos = append(repos, rest[i+1])
			i += 2
		case w == "-c" && i+1 < len(rest):
			kv := rest[i+1]
			key, val, _ := strings.Cut(kv, "=")
			if strings.EqualFold(key, hooksPathKey) {
				return "`git -c core.hooksPath=…` sends git to another hooks directory", repos
			}
			if strings.HasPrefix(strings.ToLower(key), "alias.") {
				fields := strings.Fields(val)
				aliases[key[len("alias."):]] = fields
				if mentionsHooksPath(val) {
					return "a git alias defined on the command line sets core.hooksPath", repos
				}
				for _, f := range fields {
					if isNoVerify(f) {
						return "a git alias defined on the command line adds --no-verify", repos
					}
				}
			}
			i += 2
		case strings.HasPrefix(w, "--config-env"):
			kv := strings.TrimPrefix(w, "--config-env")
			if kv == "" && i+1 < len(rest) {
				kv = rest[i+1]
				i++
			}
			if mentionsHooksPath(kv) {
				return "`git --config-env core.hooksPath=…` sends git to another hooks directory", repos
			}
			i++
		case globalValueOptions[w]:
			i += 2
		case strings.HasPrefix(w, "-"):
			i++
		default:
			goto sub
		}
	}
	return "", repos
sub:
	sub, args := rest[i], rest[i+1:]
	if fields, isAlias := aliases[sub]; isAlias && len(fields) > 0 && fields[0] == "push" {
		sub = "push" // `git -c alias.p=push p --no-verify`
	}
	switch sub {
	case "push":
		for _, a := range args {
			if isNoVerify(a) {
				return "`git push " + a + "` skips the pre-push hook that gates this push", repos
			}
		}
	case "config":
		set, operands := false, 0
		for _, a := range args {
			if readOnlyConfigFlags[a] {
				return "", repos
			}
			if !strings.HasPrefix(a, "-") {
				operands++
			}
			if mentionsHooksPath(a) {
				set = true
			}
		}
		// `git config core.hooksPath` with the key alone is a read; a value after it is a write.
		if set && operands > 1 || set && strings.Contains(strings.Join(args, " "), "hooksPath=") {
			return "`git config core.hooksPath` sends git to another hooks directory", repos
		}
	}
	return "", repos
}

// pushGateDisable judges a hookDisabling finding against the gate of every repo the command
// can touch. It refuses (in both modes, like the other ungated refusals) where the gate governs
// the repo and nothing bypasses it, and allows where the gate does not govern the repo, so a
// push with --no-verify in an unrelated repo is not this guard's business.
func pushGateDisable(cmd, why string, repos []string, first bool, cfg PushGateConfig) Result {
	if cfg.ResolveGate == nil {
		return allow
	}
	if v, ok := leadingAssignments(cmd)[reviewGateBypassVar]; ok && v != "" && first {
		return allow // a deliberate inline bypass on the command that carries it
	}
	sessionRepo, _ := resolveRepoPath("", cfg.Cwd, cfg.Home)
	targets := []string{sessionRepo}
	for _, r := range repos {
		if resolved, known := resolveRepoPath(r, cfg.Cwd, cfg.Home); known {
			if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
				targets = append(targets, resolved)
			}
		}
	}
	for _, repo := range targets {
		g := cfg.ResolveGate(repo)
		if g == nil {
			continue
		}
		mode := ModeAdvisory
		if cfg.Mode != nil {
			mode = cfg.Mode(repo)
		}
		ok, outcome, reason := g.CheckUndetermined(repo, why)
		var res Result
		if outcome == gate.OutcomeUngated {
			res = jsonDeny(pushGateDisableReason(why, g.RecordsDir, mode))
		} else {
			res = pushGateVerdict(g, mode, ok, outcome, reason, true)
		}
		if res != allow {
			return res
		}
	}
	return allow
}

// pushGateDisableReason is the deny for a command that switches the pre-push hook off.
func pushGateDisableReason(why, recordsDir string, mode GuardMode) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "Review-gate: command blocked — " + why + ". The git pre-push hook is what gates a raw `git push` after the shell has expanded it, and this would switch it off." +
		" Run the push without it, or use `leadkit push --repo <clone> --branch <branch>`, which gates in-process and needs neither." +
		"  |  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + "." +
		" [push-gate mode: " + mode.String() + "]"
}
