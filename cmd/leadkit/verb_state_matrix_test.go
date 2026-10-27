package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The verb x state matrix (ticket 105).
//
// Every instance of the ambient-profile class came from one function collapsing two states that
// call for different answers: "no profile anywhere" and "a profile that would not load"; "no
// profile" and "a profile whose origin cannot be read". The existing matrices pin WHICH project's
// profile governs a target. This one pins WHAT EACH VERB DOES in each state a target can be in, so
// that a resolver which merges two of them changes a cell and fails here, instead of arriving as
// the next ticket.
//
// It is the net the shared resolver is built inside, not an alternative to it: each verb is
// migrated onto the resolver separately, and lands with its row green.
//
// The states of a TARGET REPO, each a real git repository run through the real binary from a
// neutral directory (so the answer cannot come from the cwd):
//
//	governed     its own valid leadkit.yaml, an origin, and a records_dir of its own
//	ungoverned   no profile anywhere, and nothing in it says it is a leadkit project
//	unreadable   a leadkit.yaml that does not parse, in a repo with a .leadkit/ directory
//	mismatch     no in-repo profile; an external config declaring another repo's project_origin
//	noorigin     governed, but with no origin remote, so its origin cannot be read
//
// (The sixth state, a ticket that belongs to a store by its queue prefix with the ticket absent, is
// about stores and not repos; see verb_store_matrix_test.go.)
//
// A cell holds what the verb DOES TODAY. A cell with a Gap is a wrong or inconsistent answer that
// is pinned as it is, so that fixing it fails the row on purpose: the fix edits the row, and the
// diff shows the behaviour changing. Want is what the verb does; Desired is what it should do.

type repoState string

const (
	stGoverned   repoState = "governed"
	stUngoverned repoState = "ungoverned"
	stUnreadable repoState = "unreadable"
	stMismatch   repoState = "mismatch"
	stNoOrigin   repoState = "noorigin"
)

var allRepoStates = []repoState{stGoverned, stUngoverned, stUnreadable, stMismatch, stNoOrigin}

// stateWorld is the target repos plus the neutral directory every verb is run from and the
// HOME they share (the external config of the mismatch state lives under it).
type stateWorld struct {
	home, neutral string
	repo          map[repoState]cliRepo
}

func newStateWorld(t *testing.T) stateWorld {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := stateWorld{home: filepath.Join(root, "home"), neutral: filepath.Join(root, "neutral"), repo: map[repoState]cliRepo{}}
	for _, d := range []string{filepath.Join(w.home, ".claude", "leadkit", "config"), w.neutral} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Each profile's push_whitelist names its own state, so the whitelist that governs a push is
	// visible in the verdict: `git push origin <state>/x` is allowed only by that profile.
	prof := func(s repoState) string {
		return "queue_root: ./queue\nstore_root: ./tasks\nrecords_dir: ./recs\npush_whitelist: [" + string(s) + "/*]\n"
	}
	w.repo[stGoverned] = cliProject(t, root, "governed", "git@example.com:org/governed.git", prof(stGoverned))
	w.repo[stUngoverned] = cliProject(t, root, "ungoverned", "git@example.com:org/ungoverned.git", "")
	w.repo[stUnreadable] = cliProject(t, root, "unreadable", "git@example.com:org/unreadable.git", "queue_root: [unclosed\n")
	if err := os.MkdirAll(filepath.Join(w.repo[stUnreadable].dir, ".leadkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.repo[stMismatch] = cliProject(t, root, "mismatch", "git@example.com:org/mismatch.git", "")
	ext := "project_origin: someone/else\nqueue_root: ./queue\nstore_root: ./tasks\nrecords_dir: ./recs\n"
	if err := os.WriteFile(filepath.Join(w.home, ".claude", "leadkit", "config", "mismatch.yaml"), []byte(ext), 0o644); err != nil {
		t.Fatal(err)
	}
	w.repo[stNoOrigin] = cliProject(t, root, "noorigin", "", prof(stNoOrigin))
	return w
}

// run execs the binary from the neutral directory with the shared HOME and no $LEADKIT_PROFILE,
// so the target is the only thing that can decide the answer.
func (w stateWorld) run(t *testing.T, bin string, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = w.neutral
	cmd.Env = sandboxedEnv(w.home, []string{"LEADKIT_PROFILE="})
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %v: %v\n%s", args, err, out)
	return "", -1
}

// recordsClass says which records directory a path is: the target's own, the account default, or
// something else. Compared symlink-resolved, since macOS spells TMPDIR through /var.
func (w stateWorld) recordsClass(path string, s repoState) string {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	switch real(path) {
	case real(filepath.Join(w.repo[s].dir, "recs")):
		return "records:target"
	case real(filepath.Join(w.home, ".claude", "review-gate", "records")):
		return "records:default"
	}
	return "records:other(" + path + ")"
}

// cell is one verb in one state. Want is today's observable behaviour. A non-empty Gap marks it a
// wrong or inconsistent answer: Desired is the answer the cell should give, and Gap names where
// that is being decided.
type cell struct {
	Want    string
	Gap     string
	Desired string
}

// stateVerb is one verb's adapter: what it does to a repo in a state, reduced to a short class.
type stateVerb struct {
	name    string
	observe func(t *testing.T, w stateWorld, bin string, s repoState) string
	cells   map[repoState]cell
}

func ok(want string) cell { return cell{Want: want} }
func gap(want, gapRef, desired string) cell {
	return cell{Want: want, Gap: gapRef, Desired: desired}
}

var stateVerbs = []stateVerb{
	{
		name: "gate check",
		observe: func(t *testing.T, w stateWorld, bin string, s repoState) string {
			out, code := w.run(t, bin, "", "gate", "check", "--repo", w.repo[s].dir, "--sha", w.repo[s].sha)
			switch {
			case code == 1 && strings.Contains(out, "BLOCK"):
				return "BLOCK"
			case code == 0 && strings.Contains(out, "out of scope"):
				return "ALLOW:out-of-scope"
			case code == 0 && strings.Contains(out, "scope unresolved"):
				return "ALLOW:unresolved"
			case code == 0:
				return "ALLOW"
			}
			return fmt.Sprintf("ERROR(exit %d): %s", code, stateFirstLine(out))
		},
		cells: map[repoState]cell{
			stGoverned:   ok("BLOCK"),
			stUngoverned: ok("ALLOW:out-of-scope"),
			// 204 made push-gate refuse a repo whose profile would not load; this verb still reads the
			// built-in default and calls it out of scope, so the same repo is refused by one verb and
			// waved through by another.
			stUnreadable: gap("ALLOW:out-of-scope", "105-A", "refuse: the profile could not be read, so scope is unknown"),
			stMismatch:   gap("ALLOW:out-of-scope", "105-A", "the same answer push gives (refuse), or an explicit decision that a mismatch is out of scope"),
			// 228: the project's own origin is what puts it in scope, and it cannot be read. That is
			// unresolved scope, not out of scope. This fixture's profile sets no guard mode, so the
			// push-gate guard is advisory and an unresolved scope is allowed with its own wording; the
			// enforce-mode refusal is pinned through the binary in scope_unreadable_origin_entrypoints_test.go.
			stNoOrigin: ok("ALLOW:unresolved"),
		},
	},
	{
		name: "gate show",
		observe: func(t *testing.T, w stateWorld, bin string, s repoState) string {
			out, code := w.run(t, bin, "", "gate", "show", "--repo", w.repo[s].dir, "--sha", w.repo[s].sha)
			m := regexp.MustCompile(`(?m)^records: (.+)$`).FindStringSubmatch(out)
			if code != 0 || m == nil {
				return fmt.Sprintf("ERROR(exit %d): %s", code, stateFirstLine(out))
			}
			return w.recordsClass(strings.TrimSpace(m[1]), s)
		},
		cells: map[repoState]cell{
			stGoverned:   ok("records:target"),
			stUngoverned: ok("records:default"),
			stUnreadable: gap("records:default", "105-A", "refuse: reading the account default for a project whose own records are elsewhere answers 'no record' confidently"),
			stMismatch:   ok("records:default"),
			stNoOrigin:   ok("records:target"),
		},
	},
	{
		name: "gate record",
		observe: func(t *testing.T, w stateWorld, bin string, s repoState) string {
			out, code := w.run(t, bin, "", "gate", "record", "--repo", w.repo[s].dir, "--sha", w.repo[s].sha,
				"--layer", "code-review", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "matrix")
			if code != 0 {
				return fmt.Sprintf("ERROR(exit %d): %s", code, stateFirstLine(out))
			}
			for _, class := range []string{"target", "default"} {
				dir := filepath.Join(w.repo[s].dir, "recs")
				if class == "default" {
					dir = filepath.Join(w.home, ".claude", "review-gate", "records")
				}
				if _, err := os.Stat(filepath.Join(dir, w.repo[s].sha+".json")); err == nil {
					return "wrote:" + class
				}
			}
			return "wrote:elsewhere"
		},
		cells: map[repoState]cell{
			stGoverned:   ok("wrote:target"),
			stUngoverned: ok("wrote:default"),
			stUnreadable: gap("wrote:default", "105-A", "refuse: the record is written where nothing that gates this repo will look"),
			stMismatch:   ok("wrote:default"),
			stNoOrigin:   ok("wrote:target"),
		},
	},
	{
		name: "push",
		observe: func(t *testing.T, w stateWorld, bin string, s repoState) string {
			out, code := w.run(t, bin, "", "push", "--repo", w.repo[s].dir, "--branch", "feat/x", "--dry-run")
			return classifyPush(out, code)
		},
		cells: map[repoState]cell{
			stGoverned:   ok("BLOCK"),
			stUngoverned: ok("REFUSE:scope-unknown"),
			stUnreadable: ok("REFUSE:scope-unknown"),
			stMismatch:   ok("REFUSE:scope-unknown"),
			// 228: a profiled repo whose origin cannot be read is refused (fail-closed in every mode),
			// naming the cause rather than calling the repo out of scope.
			stNoOrigin: ok("REFUSE:scope-unresolved"),
		},
	},
	{
		name: "hook push-gate",
		observe: func(t *testing.T, w stateWorld, bin string, s repoState) string {
			out, code := w.run(t, bin, hookPayload(w.repo[s].dir, "git push origin "+w.repo[s].sha+":refs/heads/feat/x"), "hook", "push-gate")
			return classifyHook(out, code)
		},
		cells: map[repoState]cell{
			stGoverned:   ok("DENY:gate"),
			stUngoverned: ok("ALLOW"),
			stUnreadable: ok("DENY:scope-unknown"),
			stMismatch:   ok("DENY:scope-unknown"),
			// 228: this fixture's profile is advisory, and the hook allows an unresolved scope in
			// advisory mode by design. The enforce-mode DENY is pinned through the binary in
			// scope_unreadable_origin_entrypoints_test.go; the matrix has no enforce fixture.
			// The matrix has no guard-mode column, so the enforce DENY is asserted only in
			// scope_unreadable_origin_entrypoints_test.go (316).
			stNoOrigin: ok("ALLOW"),
		},
	},
	{
		name: "hook push-guard",
		observe: func(t *testing.T, w stateWorld, bin string, s repoState) string {
			out, code := w.run(t, bin, hookPayload(w.repo[s].dir, "git push origin "+string(s)+"/x"), "hook", "push-guard")
			return classifyHook(out, code)
		},
		// Each readable profile whitelists its own `<state>/*` and nothing else, so the verdict says
		// which whitelist governed: the repo's own, or the built-in default.
		cells: map[repoState]cell{
			stGoverned:   ok("ALLOW"),
			stUngoverned: ok("DENY:whitelist"),
			stUnreadable: ok("DENY:whitelist"),
			stMismatch:   ok("DENY:whitelist"),
			stNoOrigin:   ok("ALLOW"),
		},
	},
}

func stateFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func hookPayload(cwd, command string) string {
	b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": cwd, "tool_input": map[string]string{"command": command}})
	return string(b)
}

// classifyPush reduces `leadkit push --dry-run` to one of the answers a push can get.
func classifyPush(out string, code int) string {
	switch {
	case code == 1 && strings.Contains(out, "BLOCKED"):
		return "BLOCK"
	case code == 1 && strings.Contains(out, "cannot be known"):
		return "REFUSE:scope-unknown"
	case code == 1 && strings.Contains(out, "scope unresolved"):
		return "REFUSE:scope-unresolved"
	case code == 0 && strings.Contains(out, "pushing ungated"):
		return "ALLOW:ungated"
	case code == 0:
		return "ALLOW"
	}
	return fmt.Sprintf("ERROR(exit %d): %s", code, stateFirstLine(out))
}

// classifyHook reduces a PreToolUse hook's output: no output is an allow, a deny carries its reason.
func classifyHook(out string, code int) string {
	out = strings.TrimSpace(out)
	if code != 0 {
		return fmt.Sprintf("ERROR(exit %d): %s", code, stateFirstLine(out))
	}
	if out == "" {
		return "ALLOW"
	}
	var d struct {
		HookSpecificOutput struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.HookSpecificOutput.Decision != "deny" {
		return "ERROR(unrecognised output): " + stateFirstLine(out)
	}
	switch r := d.HookSpecificOutput.Reason; {
	case strings.Contains(r, "cannot be known"):
		return "DENY:scope-unknown"
	case strings.Contains(r, "push whitelist"):
		return "DENY:whitelist"
	case strings.Contains(r, "Review-gate"):
		return "DENY:gate"
	default:
		return "DENY:other(" + stateFirstLine(r) + ")"
	}
}

// TestVerbStateMatrix runs every verb against every state. A row that disagrees fails with the
// cell's own account of what it expected and why.
func TestVerbStateMatrix(t *testing.T) {
	bin := testBinary(t)
	for _, v := range stateVerbs {
		// One world per verb, not per cell: building the git repos is most of the cost. A verb's cells
		// do not disturb each other (each state is its own repo), and `gate record` only writes.
		w := newStateWorld(t)
		for _, s := range allRepoStates {
			t.Run(v.name+"/"+string(s), func(t *testing.T) {
				c, present := v.cells[s]
				if !present {
					t.Fatalf("%s has no cell for state %s: every verb must say what it does in every state", v.name, s)
				}
				got := v.observe(t, w, bin, s)
				if got == c.Want {
					return
				}
				if c.Gap != "" {
					t.Errorf("%s in state %s now gives %q, but this cell pins the known gap %s (%q, should be: %s).\n"+
						"If this is the fix, set Want to the new answer and drop the Gap; if not, something changed what the verb does.",
						v.name, s, got, c.Gap, c.Want, c.Desired)
					return
				}
				t.Errorf("%s in state %s gives %q, want %q", v.name, s, got, c.Want)
			})
		}
	}
}

// Every gap must name its decision, and every cell the verb's own answer must be one this matrix
// can tell from an error: an ERROR class pinned as the expected answer would hide a broken fixture.
func TestVerbStateMatrix_CellsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range stateVerbs {
		if seen[v.name] {
			t.Errorf("verb %q appears twice", v.name)
		}
		seen[v.name] = true
		if len(v.cells) != len(allRepoStates) {
			t.Errorf("%s has %d cells, want one per state (%d)", v.name, len(v.cells), len(allRepoStates))
		}
		for s, c := range v.cells {
			if strings.HasPrefix(c.Want, "ERROR") || c.Want == "" {
				t.Errorf("%s/%s pins %q as the expected answer", v.name, s, c.Want)
			}
			if (c.Gap == "") != (c.Desired == "") {
				t.Errorf("%s/%s: a gap needs both its reference and the desired answer (gap=%q desired=%q)", v.name, s, c.Gap, c.Desired)
			}
		}
	}
}
