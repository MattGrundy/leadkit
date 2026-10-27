package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/hookwire"
)

// HookRuntimePath is the PATH a PreToolUse hook actually executes under: a minimal
// system PATH that does NOT include the usual user bin dirs (~/.local/bin, GOPATH/bin).
//
// A command wired by bare name is resolved against this PATH, not the user's, and is not
// found when it lives in a user bin dir. Hook failures are non-blocking, so the guard stays
// configured in settings.json and enforces nothing, with no symptom: "not blocked" is also
// what success looks like.
//
// Reading settings.json cannot catch that. Only resolving the command the way the hook
// runtime will can.
const HookRuntimePath = "/usr/bin:/bin:/usr/sbin:/sbin"

// hookProbeTimeout bounds the execution probe. A wired binary that hangs is as useless
// as one that is missing, and doctor must not hang with it.
const hookProbeTimeout = 5 * time.Second

// errUnverifiable marks a command doctor cannot reduce to one binary path it can check
// (shell syntax it does not interpret, a path relative to a working directory it cannot
// know). It is a WARN, never a CRITICAL: the command may work fine in production, and
// reporting a healthy install as broken is the inverse of the bug this probe exists for.
var errUnverifiable = errors.New("cannot be verified")

// errNotProbed is returned by Probe for a binary outside the known install set. Resolution
// succeeded (it is a file that exists) but doctor declined to EXECUTE it.
var errNotProbed = errors.New("not executed")

// HookProber resolves and executes a wired hook's binary. Split behind an interface so
// the checks are testable without a real binary on disk.
type HookProber interface {
	// Resolve returns the absolute path the hook runtime would execute for token (a single
	// already-unquoted, already-expanded word), or an error describing why it would fail to
	// find it. An error wrapping errUnverifiable means "cannot tell", not "broken".
	Resolve(token string) (string, error)
	// Probe executes bin in a way that has no side effects, returning an error only when
	// it could not be started at all. It must return an error wrapping errNotProbed
	// WITHOUT executing anything when bin is not a binary doctor is willing to run.
	Probe(bin string) error
}

// hookProber is the seam the checks use. Tests replace it; the real CLI leaves it.
var hookProber HookProber = realHookProber{}

// realHookProber is the production HookProber. The zero value is the production
// configuration.
type realHookProber struct {
	// install overrides the enumeration of installed leadkit binaries that bounds what
	// Probe may execute. nil means the live environment (RealBinarySkewInput).
	install *BinarySkewInput
}

func (p realHookProber) installSet() BinarySkewInput {
	if p.install != nil {
		return *p.install
	}
	return RealBinarySkewInput()
}

// Resolve mirrors the hook runtime: an absolute path must exist and be executable; a
// bare name must resolve against HookRuntimePath, not against the caller's own PATH,
// because the caller's PATH is exactly what misleads you here. Resolving only STATS —
// nothing is executed here.
func (realHookProber) Resolve(token string) (string, error) {
	if filepath.IsAbs(token) {
		return checkExecutable(token)
	}
	if strings.ContainsRune(token, filepath.Separator) {
		// Go's LookPath, like a shell, skips PATH for any name containing a slash and
		// resolves it against the CURRENT directory. The hook runtime's directory is the
		// project's, not doctor's, so any answer given here would be about the wrong file.
		return "", fmt.Errorf("%w: %q is relative, and resolves against the hook runtime's working directory", errUnverifiable, token)
	}
	// Pure lookup rather than exec.LookPath under a temporarily rewritten PATH: mutating
	// the process-global environment to answer a question is not worth the race.
	for _, dir := range strings.Split(HookRuntimePath, string(os.PathListSeparator)) {
		if p, err := checkExecutable(filepath.Join(dir, token)); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("bare name %q does not resolve on the hook runtime's PATH (%s)", token, HookRuntimePath)
}

func checkExecutable(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("not present at %s", path)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable (mode %s)", path, fi.Mode().Perm())
	}
	return path, nil
}

// Probe runs `<bin> version`, but ONLY for a binary that is already one of the leadkit
// installs doctor knows about: a real path matching an absolute candidate enumerated by
// installCandidates (the same walk binary-skew reports on), or this very process's own
// executable. Anything else gets errNotProbed and is never started.
//
// The bound exists because the hook command comes from settings.json, and the paths doctor
// reads include $CWD/.claude/settings.json: without it, `leadkit doctor` inside a cloned
// repo would run whatever binary that repo's committed config named — on a plain
// diagnostic, where Claude Code itself gates hook execution behind a trust prompt. Probe
// runs `version` on the premise that it writes nothing; that is a property of the
// subcommand, relied on only for binaries we already trust to be leadkit.
func (p realHookProber) Probe(bin string) error {
	in := p.installSet()
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("%w: %q is not an absolute path", errNotProbed, bin)
	}
	if !installedBinary(in, in.realPath(bin)) {
		return fmt.Errorf("%w: %s is not a known leadkit install path or this binary", errNotProbed, bin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	// Run under the environment the hook runtime documents (HookRuntimePath), not the
	// caller's: resolution is runtime-faithful, so execution must be too.
	cmd.Env = []string{"PATH=" + HookRuntimePath, "HOME=" + os.Getenv("HOME")}
	if err := cmd.Run(); err != nil {
		// Check the DEADLINE FIRST. CommandContext kills the child on timeout, and
		// cmd.Run then returns an *exec.ExitError with a non-nil ProcessState — the same
		// shape as an honest non-zero exit. Testing for ExitError before ctx.Err() makes
		// a hung binary report healthy and leaves the timeout branch unreachable, which
		// is the opposite of what the comment on hookProbeTimeout promises.
		if ctx.Err() != nil {
			return fmt.Errorf("did not respond within %s", hookProbeTimeout)
		}
		// A non-zero exit still proves the binary started, which is what is being
		// probed. Only a start failure is a finding.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ProcessState != nil {
			return nil
		}
		return fmt.Errorf("could not be executed: %v", err)
	}
	return nil
}

// installedBinary reports whether real (an already-resolved path) is the real file behind
// an absolute install candidate, or behind in.Self. Relative candidates are excluded: an
// empty or relative PATH entry joins to a path under the CURRENT directory, which in a
// cloned repo is the repo's — exactly what must never count as installed.
func installedBinary(in BinarySkewInput, real string) bool {
	for _, c := range installCandidates(in) {
		if filepath.IsAbs(c.path) && filepath.IsAbs(c.real) && c.real == real {
			return true
		}
	}
	if in.Self != "" && filepath.IsAbs(in.Self) {
		return in.realPath(in.Self) == real
	}
	return false
}

var assignmentWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// hookCommandWord reduces the text a hook command has BEFORE `hook <name>` (see
// hookwire.BinaryToken) to the single word that names the binary, the way the hook
// runtime's shell will: quotes removed, `~` / $HOME / ${HOME} expanded, leading VAR=value
// assignments and a leading `exec` dropped. Those are the command shapes that work in
// production.
//
// Anything else — a pipeline, command substitution, another variable, a wrapper command,
// a glob — returns errUnverifiable rather than a guess.
func hookCommandWord(prefix, home string) (string, error) {
	words, err := splitShellWords(prefix, home)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errUnverifiable, err)
	}
	for len(words) > 0 && words[0].assignment {
		words = words[1:]
	}
	if len(words) > 0 && words[0].text == "exec" {
		words = words[1:]
	}
	if len(words) != 1 || words[0].text == "" {
		return "", fmt.Errorf("%w: %q is not a single binary word", errUnverifiable, prefix)
	}
	return words[0].text, nil
}

type shellWord struct {
	text       string
	assignment bool // began with an unquoted NAME=
}

// splitShellWords is the deliberately small subset of sh quoting that hook commands use.
func splitShellWords(s, home string) ([]shellWord, error) {
	var (
		words   []shellWord
		cur     strings.Builder
		lead    strings.Builder // literal unquoted text up to the first quote or expansion
		leadEnd bool
		inWord  bool
	)
	flush := func() {
		if inWord {
			words = append(words, shellWord{text: cur.String(), assignment: assignmentWord.MatchString(lead.String())})
		}
		cur.Reset()
		lead.Reset()
		leadEnd, inWord = false, false
	}
	literal := func(r rune) {
		cur.WriteRune(r)
		if !leadEnd {
			lead.WriteRune(r)
		}
		inWord = true
	}
	special := func(text string) { // quoted or expanded text: not part of an assignment name
		cur.WriteString(text)
		leadEnd, inWord = true, true
	}
	// expand handles a `$` at rs[i]; it returns the index of the last rune consumed.
	expand := func(rs []rune, i int) (int, error) {
		rest := string(rs[i:])
		switch {
		case strings.HasPrefix(rest, "${HOME}"):
			special(home)
			return i + len("${HOME}") - 1, nil
		case strings.HasPrefix(rest, "$HOME") && !isNameRune(rs, i+len("$HOME")):
			special(home)
			return i + len("$HOME") - 1, nil
		}
		return i, fmt.Errorf("expansion at %q is not interpreted (only $HOME and ~ are)", truncate(rest, 12))
	}

	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		case r == '\'':
			end := -1
			for j := i + 1; j < len(rs); j++ {
				if rs[j] == '\'' {
					end = j
					break
				}
			}
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			special(string(rs[i+1 : end]))
			i = end
		case r == '"':
			closed := false
			inWord = true
			leadEnd = true
			for i++; i < len(rs); i++ {
				c := rs[i]
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' && i+1 < len(rs) && strings.ContainsRune("\\\"$`", rs[i+1]) {
					special(string(rs[i+1]))
					i++
					continue
				}
				if c == '`' {
					return nil, errors.New("command substitution is not interpreted")
				}
				if c == '$' {
					var err error
					if i, err = expand(rs, i); err != nil {
						return nil, err
					}
					continue
				}
				special(string(c))
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
		case r == '\\':
			if i+1 >= len(rs) {
				return nil, errors.New("trailing backslash")
			}
			special(string(rs[i+1]))
			i++
		case r == '$':
			var err error
			if i, err = expand(rs, i); err != nil {
				return nil, err
			}
		case r == '~' && !inWord:
			if i+1 < len(rs) && rs[i+1] != '/' && rs[i+1] != ' ' {
				return nil, errors.New("~user is not interpreted (only ~ and ~/ are)")
			}
			special(home)
		case strings.ContainsRune(";&|<>()`*?[{}!#", r):
			return nil, fmt.Errorf("shell syntax %q is not interpreted", r)
		default:
			literal(r)
		}
	}
	flush()
	return words, nil
}

func isNameRune(rs []rune, i int) bool {
	if i >= len(rs) {
		return false
	}
	c := rs[i]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// hookProbeChecks turns each WIRED hook into a liveness verdict: is the command settings
// .json names actually resolvable and runnable the way the hook runtime will invoke it?
//
// Only wired hooks are probed. An unwired hook is already reported by the wiring check,
// and probing it would just restate that.
//
// One verdict is issued per distinct binary, and it names EVERY hook wired to it: several
// hooks share one binary, and a failure reported under only one would leave the others
// with no line at all.
func hookProbeChecks(statuses []hookwire.WireStatus, p HookProber) []Check {
	var checks []Check
	home := os.Getenv("HOME")
	var order []string // binary words, in first-seen order
	hooksOf := map[string][]string{}
	for _, st := range statuses {
		if !st.Wired {
			continue
		}
		prefix := hookwire.BinaryToken(st.Command, st.Name)
		if prefix == "" {
			checks = append(checks, Check{
				Name: "hook-probe", OK: false, Severity: SeverityWarn,
				Detail: fmt.Sprintf("%s: could not determine which binary %q invokes", st.Name, st.Command),
			})
			continue
		}
		token, err := hookCommandWord(prefix, home)
		if err != nil {
			checks = append(checks, unverifiable(st.Name, st.Command, err))
			continue
		}
		if _, seen := hooksOf[token]; !seen {
			order = append(order, token)
		}
		hooksOf[token] = append(hooksOf[token], st.Name)
	}

	for _, token := range order {
		names := strings.Join(hooksOf[token], ", ")
		resolved, err := p.Resolve(token)
		if errors.Is(err, errUnverifiable) {
			checks = append(checks, unverifiable(names, token, err))
			continue
		}
		if err != nil {
			checks = append(checks, Check{
				Name: "hook-probe", OK: false, Severity: SeverityCritical,
				Detail: fmt.Sprintf("%s: wired to %q but %v — the hook will fail to start, "+
					"non-blocking, so every tool call it guards proceeds unguarded. "+
					"Reinstall leadkit or rewire (`leadkit init --apply --confirm`).", names, token, err),
			})
			continue
		}
		err = p.Probe(resolved)
		switch {
		case errors.Is(err, errNotProbed):
			checks = append(checks, Check{
				Name: "hook-probe", OK: true, Severity: SeverityInfo,
				Detail: fmt.Sprintf("%s: resolves to %s; not executed — it is not a known leadkit install, "+
					"and doctor does not run binaries a settings file names", names, resolved),
			})
		case err != nil:
			checks = append(checks, Check{
				Name: "hook-probe", OK: false, Severity: SeverityCritical,
				Detail: fmt.Sprintf("%s: wired to %s but it %v — the guard is inert", names, resolved, err),
			})
		default:
			checks = append(checks, Check{
				Name: "hook-probe", OK: true, Severity: SeverityInfo,
				Detail: fmt.Sprintf("%s: resolves and runs (%s)", names, resolved),
			})
		}
	}
	return checks
}

func unverifiable(names, what string, err error) Check {
	return Check{
		Name: "hook-probe", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%s: could not verify %q (%v) — not reported as broken, because it may work in production", names, what, err),
	}
}
