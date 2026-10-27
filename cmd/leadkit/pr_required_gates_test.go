package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requiredGatesRig is a governed repo whose authority entry carries a CUSTOM required_gates set, driven
// through the real binary with a fake gh on PATH (the same seam TestCLI_PRMerge_AFailingGateBlocksTheMerge uses).
// The fake describes an open, clean, green, single-commit PR, so every other rung of both ladders passes and
// the gate is the only thing that can refuse. The forge is never reached on a refusal: the logs prove it.
type requiredGatesRig struct {
	t        *testing.T
	bin      string
	home     string
	repo     string
	head     string
	profile  string
	body     string
	stubs    string
	mergeLog string
	openLog  string
}

func newRequiredGatesRig(t *testing.T, requiredGates string) *requiredGatesRig {
	t.Helper()
	r := &requiredGatesRig{t: t, bin: testBinary(t), home: t.TempDir(), repo: t.TempDir(), stubs: t.TempDir()}
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
	commit := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.repo, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	bare := filepath.Join(t.TempDir(), "forge.git")
	git("init", "-q", "--bare", bare)
	git("remote", "add", "origin", bare)
	commit("root")
	git("branch", "-M", "main")
	git("push", "-q", "origin", "main")
	git("fetch", "-q", "origin")
	git("checkout", "-q", "-b", "feature")
	r.head = commit("the only commit beyond main")
	git("push", "-q", "origin", "feature")
	git("fetch", "-q", "origin")

	r.mergeLog = filepath.Join(r.stubs, "merges.log")
	r.openLog = filepath.Join(r.stubs, "opens.log")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  \"pr view\") case \"$*\" in *commits*) echo '{\"title\":\"t\",\"number\":9,\"commits\":[{\"messageHeadline\":\"h\",\"messageBody\":\"\"}]}' ;; " +
		"*) echo '{\"state\":\"OPEN\",\"isDraft\":false,\"mergeable\":\"MERGEABLE\",\"mergeStateStatus\":\"CLEAN\",\"headRefOid\":\"" + r.head + "\",\"headRefName\":\"feature\",\"title\":\"t\",\"baseRefName\":\"main\"}' ;; esac; exit 0 ;;\n" +
		"  \"pr checks\") exit 0 ;;\n" +
		"  \"pr merge\") echo \"$@\" >> " + r.mergeLog + "; exit 0 ;;\n" +
		"  \"pr create\") echo \"$@\" >> " + r.openLog + "; exit 0 ;;\n" +
		"  api\\ repos/*/commits/*) echo '{\"commit\":{\"message\":\"plain\"}}'; exit 0 ;;\nesac\nexit 99\n"
	if err := os.WriteFile(filepath.Join(r.stubs, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r.profile = filepath.Join(r.repo, "gates.yaml")
	// security is a review layer because the roster says so: a required_gates entry must name one.
	prof := "queue_root: queue\nstore_root: tasks\nroster:\n  - type: security\n    count: 1\n    review_layer: true\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n    can_merge: true\n    merge_grant: test grant\n    required_gates: " + requiredGates + "\n"
	if err := os.WriteFile(r.profile, []byte(prof), 0o644); err != nil {
		t.Fatal(err)
	}
	r.body = filepath.Join(r.repo, "body.md")
	if err := os.WriteFile(r.body, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *requiredGatesRig) lk(args ...string) (string, int) {
	r.t.Helper()
	c := exec.Command(r.bin, args...)
	c.Dir, c.Env = r.repo, sandboxedEnv(r.home, []string{"LEADKIT_PROFILE=" + r.profile, "PATH=" + r.stubs + ":" + os.Getenv("PATH")})
	out, err := c.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return string(out), 0
}

// record lands a ready verdict for layer. The two built-in layers go through `gate record`; `gate record`
// refuses every other name, so a third layer (or a differently named one) is written into the record file
// directly, in the shape ReadRecord parses.
func (r *requiredGatesRig) record(layer, by string) {
	r.t.Helper()
	if layer == "code-review" || layer == "critique" {
		if out, code := r.lk("gate", "record", "--repo", r.repo, "--sha", r.head, "--layer", layer, "--verdict", "ready", "--no-mutant", "test fixture", "--by", by); code != 0 {
			r.t.Fatalf("gate record %s: exit %d\n%s", layer, code, out)
		}
		return
	}
	path := filepath.Join(r.home, ".claude", "review-gate", "records", r.head+".json")
	rec := struct {
		SHA    string                       `json:"sha"`
		Layers map[string]map[string]string `json:"layers"`
	}{SHA: r.head, Layers: map[string]map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &rec); err != nil {
			r.t.Fatal(err)
		}
	}
	rec.Layers[layer] = map[string]string{"verdict": "ready", "by": by, "ts": "2026-10-09T00:00:00Z"}
	b, err := json.Marshal(rec)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func readLog(path string) string { b, _ := os.ReadFile(path); return string(b) }

// merge and open run the two verbs under test, each against a clean forge log, so a log line is proof
// the forge was reached by THIS call and not an earlier one.
func (r *requiredGatesRig) merge(extra ...string) (string, int) {
	r.t.Helper()
	_ = os.Remove(r.mergeLog)
	return r.lk(append([]string{"pr", "merge", "--pr", "9", "--repo", r.repo}, extra...)...)
}

func (r *requiredGatesRig) open(extra ...string) (string, int) {
	r.t.Helper()
	_ = os.Remove(r.openLog)
	return r.lk(append([]string{"pr", "create", "--base", "main", "--head", "feature", "--title", "t", "--body-file", r.body, "--repo", r.repo}, extra...)...)
}

// TestCLI_PRMergeAndCreate_HonourACustomRequiredGatesSet pins the CONFIGURED case of the review gate on both
// verbs, each of which makes its own prcreate.GateReady call. Ticket 158 pinned the default two-layer set;
// passing nil in place of spec.RequiredGates left everything green, because no test set required_gates at all.
//
// Each case is built so the default two-layer gate gives the OPPOSITE answer to the configured one at the
// step that matters. That is what makes it more than the default case in a new coat: a verb that dropped the
// setting would let the first refusal through (a third layer, or a differently named one, wanted but absent)
// or refuse the final allow (a trimmed set that the default would still be waiting on).
func TestCLI_PRMergeAndCreate_HonourACustomRequiredGatesSet(t *testing.T) {
	type step struct {
		record  [][2]string // layer, by: recorded before this step's assertion
		allowed bool
		reason  string // substring the refusal must carry (empty when allowed)
	}
	cases := []struct {
		name  string
		gates string
		steps []step
	}{
		{"a third layer on top of the default two", "[code-review, critique, security]", []step{
			{nil, false, "no review record"},
			{[][2]string{{"code-review", "cr-1"}, {"critique", "crit-1"}}, false, "security=MISSING"},
			{[][2]string{{"security", "sec-1"}}, true, ""},
		}},
		{"one layer under a non-default name", "[security]", []step{
			{nil, false, "no review record"},
			{[][2]string{{"code-review", "cr-1"}, {"critique", "crit-1"}}, false, "security=MISSING"},
			{[][2]string{{"security", "sec-1"}}, true, ""},
		}},
		{"a trimmed set of one built-in layer", "[code-review]", []step{
			{nil, false, "no review record"},
			{[][2]string{{"code-review", "cr-1"}}, true, ""},
		}},
	}
	for _, tc := range cases {
		for _, verb := range []string{"merge", "create"} {
			t.Run(verb+"/"+tc.name, func(t *testing.T) {
				r := newRequiredGatesRig(t, tc.gates)
				run, forge, plan, planGate := r.merge, r.mergeLog, "review-gate:  ok=", "review-gate:  ok="
				if verb == "create" {
					run, forge, plan, planGate = r.open, r.openLog, "gate:         ok=", "gate:         ok="
				}
				for i, s := range tc.steps {
					for _, lb := range s.record {
						r.record(lb[0], lb[1])
					}
					out, code := run()
					seen := readLog(forge) // before the dry-run below clears it
					planOut, _ := run("--dry-run")
					if s.allowed {
						if code != 0 || seen == "" || strings.Contains(out, "REFUSED") || !strings.Contains(planOut, planGate+"true") {
							t.Fatalf("step %d: with %s satisfied the %s must go through (exit %d, forge saw %q):\n%s\nplan:\n%s", i, tc.gates, verb, code, seen, out, planOut)
						}
						continue
					}
					if code == 0 || seen != "" || !strings.Contains(out, "REFUSED") || !strings.Contains(out, s.reason) || !strings.Contains(planOut, plan+"false") {
						t.Fatalf("step %d: with %s unsatisfied the %s must be refused by the gate naming %q and never reach the forge (exit %d, forge saw %q):\n%s\nplan:\n%s", i, tc.gates, verb, s.reason, code, seen, out, planOut)
					}
				}
			})
		}
	}
}

// TestCLI_PRMergeAndCreate_UserApprovedSubstitutesForACustomGateSet pins --user-approved on both verbs
// against a configured set. It is the one thing that may stand in for the gate, so it must (a) work when the
// configured gate is unmet, and (b) leave the refusal in force when it is absent: the plain call comes first
// in the same state, so a substitution that always applied would fail there.
func TestCLI_PRMergeAndCreate_UserApprovedSubstitutesForACustomGateSet(t *testing.T) {
	r := newRequiredGatesRig(t, "[code-review, critique, security]")
	r.record("code-review", "cr-1")
	r.record("critique", "crit-1") // the default pair is satisfied; only the configured third layer is not

	out, code := r.merge()
	if code == 0 || readLog(r.mergeLog) != "" || !strings.Contains(out, "security=MISSING") {
		t.Fatalf("precondition: an unmet third layer must refuse the merge (exit %d, forge saw %q):\n%s", code, readLog(r.mergeLog), out)
	}
	out, code = r.merge("--user-approved")
	if code != 0 || !strings.Contains(readLog(r.mergeLog), "--squash") || !strings.Contains(out, "SUBSTITUTED by --user-approved") || !strings.Contains(out, "security=MISSING") {
		t.Fatalf("--user-approved must substitute for the unmet gate on merge, saying so and showing what it overrode (exit %d, forge saw %q):\n%s", code, readLog(r.mergeLog), out)
	}

	out, code = r.open()
	if code == 0 || readLog(r.openLog) != "" || !strings.Contains(out, "security=MISSING") {
		t.Fatalf("precondition: an unmet third layer must refuse the PR-create (exit %d, forge saw %q):\n%s", code, readLog(r.openLog), out)
	}
	out, code = r.open("--user-approved")
	if code != 0 || !strings.Contains(readLog(r.openLog), "--head feature") || strings.Contains(out, "REFUSED") {
		t.Fatalf("--user-approved must substitute for the unmet gate on pr create (exit %d, forge saw %q):\n%s", code, readLog(r.openLog), out)
	}
}
