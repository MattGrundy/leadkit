package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_GateRecord_ADeclaredReviewLayerWorksEndToEnd is ticket 166's own test, through the real binary.
// A profile that declares a roster role review_layer and requires it in a grant: the layer can be
// recorded and shown in chain order, the gate is satisfied only when all three are ready, and the same
// layer WITHOUT the declaration is refused naming the layers that exist. Before the roster could name a
// layer, such a gate could be required but never satisfied.
func TestCLI_GateRecord_ADeclaredReviewLayerWorksEndToEnd(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := gitRepoNoCommits(t, "git@example.com:org/layered.git")
	recordsDir := filepath.Join(t.TempDir(), "records")
	profile := filepath.Join(repo, "leadkit.yaml")
	write := func(roster string) {
		t.Helper()
		body := "queue_root: queue\nstore_root: tasks\nrecords_dir: " + recordsDir + "\n" + roster +
			"authority:\n  org/layered:\n    can_open_pr: true\n    required_gates: [code-review, critique, security]\n"
		if err := os.WriteFile(profile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sha := scopeTestFakeSHA
	rec := func(layer, verdict, by string) (string, int) {
		return gateAs(t, bin, home, profile, "", "gate", "record", "--repo", repo, "--sha", sha, "--layer", layer, "--verdict", verdict, "--no-mutant", "test fixture", "--by", by)
	}

	write("roster:\n  - type: security\n    count: 1\n    review_layer: true\n")

	if out, code := rec("security", "ready", "sec-1"); code != 2 || !strings.Contains(out, "chained") || !strings.Contains(out, "only after code-review=ready") {
		t.Errorf("the list order is the chain order (exit %d):\n%s", code, out)
	}
	if out, code := rec("nope", "ready", "x-1"); code != 2 || !strings.Contains(out, "code-review critique security") {
		t.Errorf("an unknown layer must be refused naming the valid ones (exit %d):\n%s", code, out)
	}
	for _, s := range [][3]string{{"code-review", "ready", "cr-1"}, {"critique", "ready", "crit-1"}} {
		if out, code := rec(s[0], s[1], s[2]); code != 0 {
			t.Fatalf("%v (exit %d):\n%s", s, code, out)
		}
	}
	show := func() string {
		out, code := gateAs(t, bin, home, profile, "", "gate", "show", "--repo", repo, "--sha", sha)
		if code != 0 {
			t.Fatalf("gate show (exit %d):\n%s", code, out)
		}
		return out
	}
	if s := show(); !strings.Contains(s, "security") || !strings.Contains(s, "MISSING") || !strings.Contains(s, "next to record: security") {
		t.Errorf("gate show must list the third layer as the next to record:\n%s", s)
	}
	if out, code := gateAs(t, bin, home, profile, "", "gate", "check", "--repo", repo, "--sha", sha); code == 0 || !strings.Contains(out, "security=MISSING") {
		t.Errorf("two of three layers must not pass gate check (exit %d):\n%s", code, out)
	}
	// The security role cannot be reused for an earlier layer; a different role records it.
	if out, code := rec("security", "ready", "cr-9"); code == 0 || !strings.Contains(out, "same role") {
		t.Errorf("the code-review role recording security (exit %d):\n%s", code, out)
	}
	if out, code := rec("security", "ready", "sec-1"); code != 0 {
		t.Fatalf("security (exit %d):\n%s", code, out)
	}
	if out, code := gateAs(t, bin, home, profile, "", "gate", "check", "--repo", repo, "--sha", sha); code != 0 {
		t.Errorf("all three layers ready must pass gate check (exit %d):\n%s", code, out)
	}

	// Without the declaration, security is not a layer and the grant that requires it cannot be met.
	write("")
	if out, code := rec("security", "ready", "sec-2"); code != 2 || !strings.Contains(out, "unknown layer") || !strings.Contains(out, "code-review critique") {
		t.Errorf("an undeclared layer (exit %d):\n%s", code, out)
	}
}

// A grant that requires a layer the project does not have is told so, in words, at the PR: not as a
// "layers not ready: x=MISSING" that reads as work still to do.
func TestCLI_PRCreate_ARequiredGateNamingNoLayerSaysSo(t *testing.T) {
	r := newRequiredGatesRig(t, "[code-review, outcome-verifier]")
	r.record("code-review", "cr-1")
	r.record("critique", "crit-1")
	out, code := r.open()
	if code == 0 || readLog(r.openLog) != "" {
		t.Fatalf("a gate nothing can satisfy must refuse (exit %d, forge saw %q):\n%s", code, readLog(r.openLog), out)
	}
	for _, want := range []string{"required_gates cannot be satisfied", `"outcome-verifier" is not a review layer`, "valid layers"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if m, mcode := r.merge(); mcode == 0 || !strings.Contains(m, `"outcome-verifier" is not a review layer`) {
		t.Errorf("pr merge must say the same (exit %d):\n%s", mcode, m)
	}
}

// A grant that names a layer the project does not have must not change what `gate show` and `gate check`
// ask for: they stay on the default pair, so a layer no record can carry is never demanded. The grant's
// problem is said in words where the grant is used (see ARequiredGateNamingNoLayerSaysSo), not here.
// Dropping the validity test in reviewGateResolver would make these demand `typo=MISSING` forever.
func TestCLI_GateShowAndCheck_AGrantNamingNoLayerLeavesTheDefaultPair(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := gitRepoNoCommits(t, "git@example.com:org/layered.git")
	profile := filepath.Join(repo, "leadkit.yaml")
	body := "queue_root: queue\nstore_root: tasks\nrecords_dir: " + filepath.Join(t.TempDir(), "records") + "\n" +
		"authority:\n  org/layered:\n    can_open_pr: true\n    required_gates: [code-review, typo]\n"
	if err := os.WriteFile(profile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := scopeTestFakeSHA
	for _, s := range [][3]string{{"code-review", "ready", "cr-1"}, {"critique", "ready", "crit-1"}} {
		if out, code := gateAs(t, bin, home, profile, "", "gate", "record", "--repo", repo, "--sha", sha, "--layer", s[0], "--verdict", s[1], "--no-mutant", "test fixture", "--by", s[2]); code != 0 {
			t.Fatalf("%v (exit %d):\n%s", s, code, out)
		}
	}
	out, code := gateAs(t, bin, home, profile, "", "gate", "show", "--repo", repo, "--sha", sha)
	if code != 0 || strings.Contains(out, "typo") || !strings.Contains(out, "chain: satisfied") {
		t.Errorf("gate show must stay on the default pair and be satisfied (exit %d):\n%s", code, out)
	}
	if out, code := gateAs(t, bin, home, profile, "", "gate", "check", "--repo", repo, "--sha", sha); code != 0 || strings.Contains(out, "typo") {
		t.Errorf("gate check must not demand a layer no record can carry (exit %d):\n%s", code, out)
	}
}

// `gate verdict` is the verb owners use to record and route, and it must see the layers the roster declares
// and the chain the grant requires: the layer is recorded, and a ready on a layer that is not the last
// hands the ticket to the next layer's lane. The review package tests pass their own Gate, so only this
// drives the CLI's own choice of resolver.
func TestCLI_GateVerdict_RecordsADeclaredLayerAndRoutesTheChain(t *testing.T) {
	r := newOwnerRig(t)
	profile := "queue_root: queue\nstore_root: tasks\nenforced_remotes: [based]\n" +
		"roster:\n  - type: developer\n    count: 1\n  - type: security\n    count: 1\n    review_layer: true\n" +
		"authority:\n  org/based:\n    can_open_pr: true\n    required_gates: [code-review, critique, security]\n"
	if err := os.WriteFile(strings.TrimPrefix(r.env[0], "LEADKIT_PROFILE="), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.queue, "security", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, path := r.newTicket("developer")
	r.sendToReview(path)
	base := filepath.Base(path)
	lane := func(name string) string { return filepath.Join(r.queue, name, "pending", base) }

	verdict := func(from, layer, by string) {
		t.Helper()
		out, code := r.lk("gate", "verdict", "--ticket", lane(from), "--verdict", "ready", "--no-mutant", "test fixture", "--layer", layer, "--by", by)
		if code != 0 {
			t.Fatalf("gate verdict --layer %s (exit %d):\n%s", layer, code, out)
		}
	}
	verdict("code-review", "code-review", "cr-1")
	if _, err := os.Lstat(lane("critique")); err != nil {
		t.Fatalf("code-review ready must hand the ticket to the critique lane: %v", err)
	}
	verdict("critique", "critique", "crit-1")
	// Critique is not the last layer of this chain: the declared layer's lane is next, not finalise.
	if _, err := os.Lstat(lane("security")); err != nil {
		t.Fatalf("critique ready with security still required must hand the ticket to the security lane: %v", err)
	}
	if _, err := os.Lstat(lane("finalise")); err == nil {
		t.Fatal("the ticket must not reach finalise before the security layer")
	}
	verdict("security", "security", "sec-1")

	out, code := r.lk("gate", "show", "--repo", r.repo, "--sha", r.sha)
	if code != 0 || !strings.Contains(out, "security") || !strings.Contains(out, "chain: satisfied") {
		t.Errorf("the declared layer must be recorded and the chain satisfied (exit %d):\n%s", code, out)
	}
	if fm := readFile(t, path); !strings.Contains(fm, "stage: finalise") {
		t.Errorf("the last layer ready must route the ticket to finalise:\n%s", fm)
	}
}
