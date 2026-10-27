package hook

import (
	"strings"
	"testing"
)

// Ticket 302, objective 3. A heredoc body that mentions push must not be refused for a wrapper
// elsewhere on the command line that the body cannot reach, and must still be refused for every
// route by which it CAN reach one. The two tables are the two halves of one rule and are meant to
// be read together.

func heredocPushState(cmd string) pushParse { return parsePushCommands(cmd) }

// The body names push; a wrapper runs elsewhere on the line and nothing connects the two.
func TestHeredocPush_AWrapperTheBodyCannotReachIsNotACauseToRefuse(t *testing.T) {
	for name, cmd := range map[string]string{
		"commit message then a python3 pipeline":     "git commit -F - <<'EOF'\nfix: the push gate\nEOF\ngh pr list --json a | python3 -c 'print(1)'",
		"commit message in $( ) then a python3 pipe": "git commit -m \"$(cat <<'EOF'\nthe push gate\nEOF\n)\" && gh pr list | python3 -c 'x'",
		"cat then an xargs pipeline":                 "cat <<'EOF'\nthe push gate\nEOF\necho x | xargs echo",
		"head then a find pipeline":                  "cat <<'EOF' | head -3\nthe push gate\nEOF\nfind . -name x | wc -l",
		"cat then env":                               "cat <<'EOF'\nthe push gate\nEOF\nenv FOO=1 make",
		"awk pipeline first, heredoc second":         "echo a | awk '{print}'; cat <<'EOF'\nthe push gate\nEOF",
		"stderr redirect is not a kept stdout":       "cat <<'EOF' 2>&1 | head\nthe push gate\nEOF\nxargs echo < list",
		"wrapper after an || list":                   "cat <<'EOF'\nthe push gate\nEOF\ntrue || bash -c 'make'",
		"wrapper joined by ||":                       "cat <<'EOF' || bash -c 'make'\nthe push gate\nEOF",
		"wrapper joined by &&":                       "cat <<'EOF' && xargs echo < list\nthe push gate\nEOF",
		"wrapper joined by ;":                        "cat <<'EOF'; env FOO=1 make\nthe push gate\nEOF",
	} {
		t.Run(name, func(t *testing.T) {
			if p := heredocPushState(cmd); p.State != pushNone {
				t.Errorf("a wrapper the heredoc cannot reach must not make %q unsure, got state %v: %s", cmd, p.State, p.Why)
			}
		})
	}
}

// Every way the body can reach a wrapper, or be kept for one, still refuses. A mutant that
// reads "no connection" where the body is connected fails here.
func TestHeredocPush_ARouteFromTheBodyToAWrapperStillRefuses(t *testing.T) {
	for name, cmd := range map[string]string{
		"piped straight into a shell":                      "cat <<'EOF' | sh\nthe push gate\nEOF",
		"piped into xargs":                                 "cat <<'EOF' | xargs echo\nthe push gate\nEOF",
		"piped through a second stage":                     "cat <<'EOF' | cat | sh\nthe push gate\nEOF",
		"piped with |&":                                    "cat <<'EOF' |& sh\nthe push gate\nEOF",
		"shell before the heredoc stage":                   "bash -c 'x' | cat <<'EOF'\nthe push gate\nEOF",
		"written to a file a shell runs":                   "cat > run.sh <<'EOF'\nthe push gate\nEOF\nbash run.sh",
		"appended to a file":                               "cat >> run.sh <<'EOF'\nthe push gate\nEOF\nbash run.sh",
		"tee keeps it":                                     "cat <<'EOF' | tee f && sh f\nthe push gate\nEOF",
		"tee in front of a shell":                          "cat <<'EOF' | tee f | sh\nthe push gate\nEOF",
		"a filter that may write a file":                   "cat <<'EOF' | sed 's/a/b/' > f\nthe push gate\nEOF\nsh f",
		"sort -o keeps it":                                 "cat <<'EOF' | sort -o f\nthe push gate\nEOF\nsh f",
		"sort -ro keeps it":                                "cat <<'EOF' | sort -ro f\nthe push gate\nEOF\nsh f",
		"a stage the reading cannot call inert":            "cat <<'EOF' | sponge f\nthe push gate\nEOF\nsh f",
		"kept in a variable, then run":                     "X=$(cat <<'EOF'\nthe push gate\nEOF\n); sh -c \"$X\"",
		"kept by a backtick substitution":                  "X=`cat <<'EOF'\nthe push gate\nEOF\n`; eval \"$X\"",
		"a brace group piped into a shell":                 "{ cat <<'EOF'\nthe push gate\nEOF\n} | sh",
		"a subshell piped into a shell":                    "( cat <<'EOF'\nthe push gate\nEOF\n) | sh",
		"a loop that evals each line":                      "cat <<'EOF' | while read l; do eval \"$l\"; done\nthe push gate\nEOF",
		"a $( ) body handed to a shell":                    "sh -c \"$(cat <<'EOF'\nthe push gate\nEOF\n)\"",
		"a $( ) that itself pipes to a shell":              "git commit -m \"$(cat <<'EOF' | sh\nthe push gate\nEOF\n)\" && python3 -c 'x'",
		"a dynamic command word in the pipe":               "cat <<'EOF' | $RUNNER\nthe push gate\nEOF\nbash -c x",
		"process substitution":                             "bash <(cat <<'EOF'\nthe push gate\nEOF\n)",
		"a subshell whose output goes to a file":           "( cat <<'EOF'\nthe push gate\nEOF\n) > f\nsh f",
		"an unrelated substitution forfeits the narrowing": "cat <<'EOF'\nthe push gate\nEOF\necho \"$(date)\" | xargs echo",
	} {
		t.Run(name, func(t *testing.T) {
			p := heredocPushState(cmd)
			if p.State != pushUnsure {
				t.Fatalf("%q reaches a wrapper and must stay unsure, got state %v", cmd, p.State)
			}
		})
	}
}

// The refusal is the heredoc rule's, not an accident of another one: its reason names the wrapper.
func TestHeredocPush_TheRefusalNamesTheWrapper(t *testing.T) {
	p := heredocPushState("cat <<'EOF' | sh\nthe push gate\nEOF")
	if !strings.Contains(p.Why, "also runs a wrapper") {
		t.Errorf("want the wrapper refusal, got %q", p.Why)
	}
}

// A body that really holds a push is refused whatever surrounds it: the invariant check is
// independent of this rule and the narrowing must not reach it.
func TestHeredocPush_ABodyThatHoldsAPushIsStillRefusedWithNoWrapperAtAll(t *testing.T) {
	for _, cmd := range []string{
		"cat <<'EOF'\ngit push origin main\nEOF",
		"git commit -F - <<'EOF'\nrun git push | tee log\nEOF\nenv X=1 make",
	} {
		if p := heredocPushState(cmd); p.State != pushUnsure {
			t.Errorf("%q: a body holding `git push` must stay unsure, got %v", cmd, p.State)
		}
	}
}

// Through the hook, as the reporter hit it: the command is allowed outright, not just parsed
// as having no push.
func TestHeredocPush_TheHookAllowsTheReportedShape(t *testing.T) {
	cmd := "git commit -F - <<'EOF'\nfix: the push gate | pipe\nEOF\ngh pr list --json a | python3 -c 'print(1)'"
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote)
	assertAllow(t, dispatchPush(t, cfg, cmd), "an unreachable wrapper")
	assertDeny(t, dispatchPush(t, cfg, "cat <<'EOF' | sh\nthe push gate\nEOF"), "a reachable one")
}

func TestRedirectsStdout(t *testing.T) {
	for op, want := range map[string]bool{
		">": true, ">>": true, ">|": true, "&>": true, "&>>": true, "1>": true, "1>>": true, ">&2": true, ">&": true,
		"2>": false, "2>>": false, "2>&1": false, "<": false, "<<": false, "<<<": false, "<&3": false,
	} {
		if got := redirectsStdout(op); got != want {
			t.Errorf("redirectsStdout(%q) = %v, want %v", op, got, want)
		}
	}
}

func TestIsHeredocOperator(t *testing.T) {
	for op, want := range map[string]bool{"<<": true, "0<<": true, "<<<": false, "<": false, ">>": false} {
		if got := isHeredocOperator(op); got != want {
			t.Errorf("isHeredocOperator(%q) = %v, want %v", op, got, want)
		}
	}
}

// A body the segment reading cannot account for is one it has lost; whatever surrounds it, the
// answer is "may". (The lexer and the heredoc stripper agree on every input tried, so this is
// asserted on the function, not found by a command.)
func TestHeredocPush_ABodyTheReadingCannotAccountForIsReachable(t *testing.T) {
	segs, bodies, _ := splitSegments("cat <<'EOF'\nthe push gate\nEOF\n")
	if len(bodies) != 1 || wrapperMayRunBody(segs, len(bodies)) {
		t.Fatalf("control: one heredoc, one body, nothing to reach it: bodies=%d", len(bodies))
	}
	if !wrapperMayRunBody(segs, len(bodies)+1) {
		t.Error("a body no segment opened must be treated as reachable")
	}
}
