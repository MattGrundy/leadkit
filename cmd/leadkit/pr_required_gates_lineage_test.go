package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newStackedRequiredGatesRig is newRequiredGatesRig with a STACK: a branch "under" (one commit beyond main)
// beneath the PR head, which is a second commit on top of it. A lone commit beyond main needs no lineage
// record, so on the single-commit rig the lineage decision cannot be exercised at all. Returns the rig, with
// r.head now the stacked head, and the sha of the branch beneath it.
func newStackedRequiredGatesRig(t *testing.T, requiredGates string) (*requiredGatesRig, string) {
	t.Helper()
	r := newRequiredGatesRig(t, requiredGates)
	oldHead := r.head
	gitEnv := append(sandboxedEnv(r.home, nil), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = r.repo, gitEnv
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("branch", "under", oldHead)
	git("push", "-q", "origin", "under")
	if err := os.WriteFile(filepath.Join(r.repo, "f"), []byte("the stacked head"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "the stacked head")
	r.head = git("rev-parse", "HEAD")
	git("push", "-q", "origin", "feature")
	git("fetch", "-q", "origin")

	// The fake gh reports the head it was written with; point it at the stacked one.
	gh := filepath.Join(r.stubs, "gh")
	b, err := os.ReadFile(gh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), oldHead) {
		t.Fatal("the fake gh does not carry the head sha to replace")
	}
	if err := os.WriteFile(gh, []byte(strings.ReplaceAll(string(b), oldHead, r.head)), 0o755); err != nil {
		t.Fatal(err)
	}
	return r, oldHead
}

// recordAt lands a ready verdict for layer on sha rather than on the PR head.
func (r *requiredGatesRig) recordAt(sha, layer, by string) {
	r.t.Helper()
	head := r.head
	r.head = sha
	defer func() { r.head = head }()
	r.record(layer, by)
}

// TestCLI_PRMergeAndCreate_LineageHonoursACustomRequiredGatesSet pins the configured gate set where it
// governs the LINEAGE decision, on both verbs (each has its own prcreate.CheckLineage call). Passing nil in
// place of spec.RequiredGates at either call site left the whole suite green: the gate decision was pinned
// (see TestCLI_PRMergeAndCreate_HonourACustomRequiredGatesSet) and the lineage one was not.
//
// The PR head always satisfies the configured set, so the head's own gate passes and the lineage check, which
// reads the branch beneath, is the only rung that can refuse. Each case is built so the default two-layer set
// gives the OPPOSITE lineage answer to the configured one at the step that matters: a verb that dropped the
// setting would wave the first refusal through (the branch beneath lacks a configured third layer), or refuse
// the final allow (the branch beneath has all a trimmed set asks for, but not what the default also wants).
func TestCLI_PRMergeAndCreate_LineageHonoursACustomRequiredGatesSet(t *testing.T) {
	type step struct {
		head    [][2]string // layer, by: recorded on the PR head before this step's assertion
		under   [][2]string // layer, by: recorded on the branch beneath the head
		allowed bool
	}
	cases := []struct {
		name  string
		gates string
		steps []step
	}{
		{"a trimmed set of one built-in layer", "[code-review]", []step{
			{[][2]string{{"code-review", "cr-1"}}, nil, false},
			{nil, [][2]string{{"code-review", "cr-2"}}, true}, // the default set would still want critique beneath
		}},
		{"a third layer on top of the default two", "[code-review, critique, security]", []step{
			{[][2]string{{"code-review", "cr-1"}, {"critique", "crit-1"}, {"security", "sec-1"}}, [][2]string{{"code-review", "cr-2"}, {"critique", "crit-2"}}, false}, // the default set is satisfied beneath
			{nil, [][2]string{{"security", "sec-2"}}, true},
		}},
	}
	for _, tc := range cases {
		for _, verb := range []string{"merge", "create"} {
			t.Run(verb+"/"+tc.name, func(t *testing.T) {
				r, under := newStackedRequiredGatesRig(t, tc.gates)
				run, forge := r.merge, r.mergeLog
				if verb == "create" {
					run, forge = r.open, r.openLog
				}
				for i, s := range tc.steps {
					for _, lb := range s.head {
						r.record(lb[0], lb[1])
					}
					for _, lb := range s.under {
						r.recordAt(under, lb[0], lb[1])
					}
					out, code := run()
					seen := readLog(forge)
					if s.allowed {
						if code != 0 || seen == "" || strings.Contains(out, "REFUSED") {
							t.Fatalf("step %d: with %s satisfied on the head and beneath it the %s must go through (exit %d, forge saw %q):\n%s", i, tc.gates, verb, code, seen, out)
						}
						continue
					}
					if code == 0 || seen != "" || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "lineage is not") || !strings.Contains(out, "ungated branch") {
						t.Fatalf("step %d: with %s unsatisfied beneath the head the %s must be refused by the LINEAGE check and never reach the forge (exit %d, forge saw %q):\n%s", i, tc.gates, verb, code, seen, out)
					}
				}
			})
		}
	}
}
