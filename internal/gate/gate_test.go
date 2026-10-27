package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit is a stub Git for tests — no real git repo, no real records dir.
type fakeGit struct {
	revParse map[string]string // ref -> full sha; a ref missing here fails (ok=false)
	remote   string
}

func (f fakeGit) RevParse(repo, ref string) (string, bool) {
	sha, ok := f.revParse[ref]
	if !ok {
		// A commit peels to itself, so `X^{commit}` resolves wherever X does; a test that wants a tag
		// or a non-commit lists the peeled form explicitly (or leaves it out to fail).
		if base, found := strings.CutSuffix(ref, "^{commit}"); found {
			sha, ok = f.revParse[base]
		}
	}
	return sha, ok
}

func (f fakeGit) RemoteURL(repo string) string { return f.remote }

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newTestGate(t *testing.T, remote string) (*Gate, fakeGit) {
	t.Helper()
	fg := fakeGit{revParse: map[string]string{"HEAD": testSHA, testSHA: testSHA}, remote: remote}
	// ProjectRoot set (non-empty) so the own-origin default puts the gate IN scope by
	// default — the stub reports the same origin for the project root and the target
	// (the common "the target repo IS the profile's own project" case). Tests that need
	// an OUT-of-scope gate clear g.ProjectRoot (unprofiled) or set an explicit
	// EnforcedRemotes list the origin doesn't match.
	return &Gate{RecordsDir: t.TempDir(), Git: fg, ProjectRoot: "/project-root"}, fg
}

func TestCanonicalRole(t *testing.T) {
	tests := map[string]string{
		"cr-1":                 "cr",
		"cr-1-3":               "cr",
		"cr-relief":            "cr", // matches "cr-*" prefix directly, no need for the trailing-suffix strip
		"code-review-agent":    "cr",
		"cr":                   "cr",
		"critique-agent":       "crit",
		"crit-2":               "crit",
		"crit-2r":              "crit", // matches "crit-*" prefix directly
		"crit":                 "crit",
		"outcome-verifier":     "outcome-verifier", // retired from the built-in families: a role like any other
		"ov-1":                 "ov",
		"security-agent":       "security-agent", // a project-declared review role is its own family
		"security-agent-2":     "security-agent",
		"finalise":             "finalise",
		"finalise-3":           "finalise", // no cr/crit/ov prefix match; default branch strips trailing "-3"
		"crit-deadline-relief": "crit",     // matches "crit-*" prefix directly
	}
	for in, want := range tests {
		if got := CanonicalRole(in); got != want {
			t.Errorf("CanonicalRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecord_ChainingRefusesCritiqueBeforeCodeReview(t *testing.T) {
	g, _ := newTestGate(t, "")
	_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"})
	if err == nil {
		t.Fatal("want refusal recording critique before code-review, got nil")
	}
	if !strings.Contains(err.Error(), "chained") {
		t.Errorf("error = %q, want it to mention chaining", err.Error())
	}
}

func TestRecord_WriteOnce(t *testing.T) {
	g, _ := newTestGate(t, "")
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatalf("first record: %v", err)
	}

	t.Run("same verdict again is idempotent", func(t *testing.T) {
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
			t.Errorf("re-recording the same verdict should be OK, got: %v", err)
		}
	})

	// Asymmetric write-once: a changes-needed dissent over a prior ready is now ALLOWED
	// (it supersedes — see TestRecord_DissentSupersedesReady); only the reverse (ready
	// over a changes-needed) stays refused (TestRecord_ReadyOverDissentStillRefused).
	t.Run("a changes-needed dissent supersedes a prior ready", func(t *testing.T) {
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-2"}); err != nil {
			t.Fatalf("a dissent must supersede a prior ready, got: %v", err)
		}
	})
}

// TestRecord_DissentSupersedesReady is the #123 near-miss regression: a stale `ready`
// code-review record must NOT be able to refuse a later `changes-needed` dissent on the
// same sha. Before the fix, symmetric write-once REFUSED this (fails-before); after,
// the dissent supersedes, the record reads changes-needed, and Check BLOCKS.
func TestRecord_DissentSupersedesReady(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")

	// A stale `ready` sits on code-review/X (as it did for sha 48de3005).
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-stale"}); err != nil {
		t.Fatalf("seed stale ready: %v", err)
	}

	// A later reviewer finds a real blocking bug and records changes-needed — this MUST
	// be allowed (before the fix it was refused, masking the bug).
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-new"}); err != nil {
		t.Fatalf("dissent must be recordable over a stale ready, got: %v", err)
	}

	// The record now reads the dissent, not the stale ready.
	rec, err := g.ReadRecord(testSHA)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	cr := rec.Layers["code-review"]
	if cr.Verdict != "changes-needed" {
		t.Fatalf("code-review verdict = %q, want changes-needed", cr.Verdict)
	}
	if cr.By != "cr-new" {
		t.Errorf("code-review by = %q, want cr-new (the dissenter)", cr.By)
	}

	// Audit: the superseded prior verdict + author are preserved.
	if cr.SupersededVerdict != "ready" || cr.SupersededBy != "cr-stale" {
		t.Errorf("audit not preserved: SupersededVerdict=%q SupersededBy=%q, want ready/cr-stale", cr.SupersededVerdict, cr.SupersededBy)
	}

	// The gate now BLOCKS on the dissent (check-blocks-on-dissent).
	ok, reason := g.Check(testSHA, "")
	if ok {
		t.Fatalf("want BLOCK after a changes-needed dissent, got allow: %s", reason)
	}
	if !strings.Contains(reason, "code-review=changes-needed") {
		t.Errorf("reason = %q, want it to show code-review=changes-needed", reason)
	}
}

// TestRecord_ReadyOverDissentStillRefused locks the controlled direction: un-blocking a
// changes-needed by writing `ready` in place on the SAME sha stays refused (rework is a
// new sha, never a silent in-place flip).
func TestRecord_ReadyOverDissentStillRefused(t *testing.T) {
	g, _ := newTestGate(t, "")
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-1"}); err != nil {
		t.Fatalf("seed changes-needed: %v", err)
	}
	_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2"})
	if err == nil {
		t.Fatal("want REFUSED flipping a changes-needed to ready in place, got nil")
	}
	if !strings.Contains(err.Error(), "NEW sha") {
		t.Errorf("error = %q, want it to explain un-block needs a new sha", err.Error())
	}
	// The dissent is untouched.
	rec, _ := g.ReadRecord(testSHA)
	if rec.Layers["code-review"].Verdict != "changes-needed" {
		t.Errorf("verdict = %q, want changes-needed to survive the refused flip", rec.Layers["code-review"].Verdict)
	}
}

// TestRecord_DissentIdempotentPreservesAudit checks recording the same changes-needed
// twice is a no-op that does NOT wipe the superseded-audit trail from the first dissent.
func TestRecord_DissentIdempotentPreservesAudit(t *testing.T) {
	g, _ := newTestGate(t, "")
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-stale"}); err != nil {
		t.Fatalf("seed ready: %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-new"}); err != nil {
		t.Fatalf("dissent: %v", err)
	}
	// Same verdict again by the same recorder — idempotent.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-new"}); err != nil {
		t.Fatalf("re-recording the same verdict should be OK, got: %v", err)
	}
	rec, _ := g.ReadRecord(testSHA)
	cr := rec.Layers["code-review"]
	if cr.SupersededVerdict != "ready" || cr.SupersededBy != "cr-stale" {
		t.Errorf("audit wiped by idempotent re-record: SupersededVerdict=%q SupersededBy=%q, want ready/cr-stale", cr.SupersededVerdict, cr.SupersededBy)
	}
}

// TestRecord_ChangesNeededRecordableRegardlessOfChain: a blocking finding must never be
// gated behind the very chain it blocks. A changes-needed on critique is recordable even
// when code-review is unrecorded/not-ready; the chain rule stays only for a ready critique.
func TestRecord_ChangesNeededRecordableRegardlessOfChain(t *testing.T) {
	t.Run("changes-needed on critique with code-review unrecorded", func(t *testing.T) {
		g, _ := newTestGate(t, "")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "changes-needed", By: "crit-1"}); err != nil {
			t.Fatalf("a critique dissent must be recordable regardless of the chain, got: %v", err)
		}
		ok, _ := g.Check(testSHA, "github.com/org/web-client")
		// (Check uses the gate's remote; recorded with an empty remote gate, so re-check
		// via a scoped gate would need the same records dir — instead just confirm the
		// record shows the dissent.)
		_ = ok
		rec, _ := g.ReadRecord(testSHA)
		if rec.Layers["critique"].Verdict != "changes-needed" {
			t.Errorf("critique verdict = %q, want changes-needed", rec.Layers["critique"].Verdict)
		}
	})

	t.Run("changes-needed on critique with code-review itself changes-needed", func(t *testing.T) {
		g, _ := newTestGate(t, "")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-1"}); err != nil {
			t.Fatalf("code-review dissent: %v", err)
		}
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "changes-needed", By: "crit-1"}); err != nil {
			t.Fatalf("critique dissent must be recordable even with the chain fully unsatisfied, got: %v", err)
		}
	})

	t.Run("a ready critique is STILL chained (regression)", func(t *testing.T) {
		g, _ := newTestGate(t, "")
		_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"})
		if err == nil || !strings.Contains(err.Error(), "chained") {
			t.Fatalf("want a ready critique still refused as chained, got: %v", err)
		}
	})
}

// TestRecord_DissentInvalidatesSiblingReady is Fix 2: a code-review changes-needed on a
// sha whose critique was ready must leave the gate NOT satisfied, and the sibling ready
// is demoted (with its audit preserved).
func TestRecord_DissentInvalidatesSiblingReady(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatalf("code-review ready: %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatalf("critique ready: %v", err)
	}
	// Both ready → gate satisfied.
	if ok, reason := g.Check(testSHA, ""); !ok {
		t.Fatalf("precondition: want all-layers-ready to pass, got: %s", reason)
	}

	// A later code-review dissent lands.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-2"}); err != nil {
		t.Fatalf("code-review dissent: %v", err)
	}

	// Gate no longer satisfied.
	if ok, reason := g.Check(testSHA, ""); ok {
		t.Fatalf("want BLOCK after the dissent, got allow: %s", reason)
	}

	// The sibling critique `ready` is superseded (needs-re-review), audit preserved.
	rec, _ := g.ReadRecord(testSHA)
	crit := rec.Layers["critique"]
	if crit.Verdict == "ready" {
		t.Errorf("sibling critique still reads ready — it was not invalidated by the dissent")
	}
	if crit.SupersededVerdict != "ready" || crit.SupersededBy != "crit-1" {
		t.Errorf("sibling audit not preserved: SupersededVerdict=%q SupersededBy=%q, want ready/crit-1", crit.SupersededVerdict, crit.SupersededBy)
	}
}

func TestRecord_LayerIndependence(t *testing.T) {
	t.Run("same role as code-review cannot record critique", func(t *testing.T) {
		g, _ := newTestGate(t, "")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1-3"}); err != nil {
			t.Fatalf("code-review record: %v", err)
		}
		// cr-relief canonicalizes to "cr", same role as cr-1-3.
		_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "cr-relief"})
		if err == nil {
			t.Fatal("want refusal — same role recording both layers, got nil")
		}
	})

	t.Run("a cr-role agent may never record critique, even against a different by", func(t *testing.T) {
		g, _ := newTestGate(t, "")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "code-review-agent"}); err != nil {
			t.Fatalf("code-review record: %v", err)
		}
		_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "cr-2"})
		if err == nil {
			t.Fatal("want refusal — a cr-role by can never record critique, got nil")
		}
	})

	t.Run("a different role may record critique", func(t *testing.T) {
		g, _ := newTestGate(t, "")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
			t.Fatalf("code-review record: %v", err)
		}
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
			t.Errorf("want a different-role critique to succeed, got: %v", err)
		}
	})
}

func TestCheck_States(t *testing.T) {
	t.Run("no record blocks", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/web-client")
		ok, reason := g.Check(testSHA, "")
		if ok {
			t.Fatal("want BLOCK for a missing record")
		}
		if !strings.Contains(reason, "no review record") {
			t.Errorf("reason = %q, want it to contain %q", reason, "no review record")
		}
	})

	t.Run("partial (code-review only) blocks and names the missing layer", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/web-client")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
			t.Fatalf("record: %v", err)
		}
		ok, reason := g.Check(testSHA, "")
		if ok {
			t.Fatal("want BLOCK with only one layer ready")
		}
		if !strings.Contains(reason, "layers not ready") || !strings.Contains(reason, "critique=") {
			t.Errorf("reason = %q, want it to name critique as not ready", reason)
		}
	})

	t.Run("all layers ready allows", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/web-client")
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
			t.Fatalf("record code-review: %v", err)
		}
		if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
			t.Fatalf("record critique: %v", err)
		}
		ok, reason := g.Check(testSHA, "")
		if !ok {
			t.Fatalf("want OK with all layers ready, got BLOCK: %s", reason)
		}
		if !strings.Contains(reason, "all layers ready") {
			t.Errorf("reason = %q, want it to contain %q", reason, "all layers ready")
		}
	})

	t.Run("out-of-scope (unprofiled) repo allows even with no record", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/some-other-repo")
		g.ProjectRoot = "" // unprofiled target: no explicit list, no own origin → out of scope
		ok, _ := g.Check(testSHA, "")
		if !ok {
			t.Fatal("want allow for an out-of-scope (unprofiled) repo")
		}
	})

	t.Run("REVIEW_GATE_BYPASS env allows", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/platform-infra")
		t.Setenv("REVIEW_GATE_BYPASS", "testing")
		ok, reason := g.Check(testSHA, "")
		if !ok {
			t.Fatal("want allow with REVIEW_GATE_BYPASS set")
		}
		if !strings.Contains(reason, "BYPASS") {
			t.Errorf("reason = %q, want it to mention BYPASS", reason)
		}
	})

	t.Run("BYPASS file allows", func(t *testing.T) {
		g, _ := newTestGate(t, "github.com/org/platform-infra")
		if err := os.WriteFile(filepath.Join(g.RecordsDir, "BYPASS"), []byte("incident 123"), 0o644); err != nil {
			t.Fatal(err)
		}
		ok, reason := g.Check(testSHA, "")
		if !ok {
			t.Fatal("want allow with a BYPASS file present")
		}
		if !strings.Contains(reason, "BYPASS") {
			t.Errorf("reason = %q, want it to mention BYPASS", reason)
		}
	})
}

func TestCheckPush_SkipsAllZeroSHA(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	// A branch-delete row (all-zero local sha) must be skipped, not blocked, even
	// though there's no record for it and the remote is in scope.
	stdin := strings.NewReader("refs/heads/gone 0000000000000000000000000000000000000000 refs/heads/gone " + testSHA + "\n")
	ok, err := g.CheckPush(stdin, "")
	if err != nil {
		t.Fatalf("CheckPush: %v", err)
	}
	if !ok {
		t.Error("want pass — the only row is an all-zero (branch delete) local sha")
	}
}

func TestCheckPush_BlocksOnAnyFailingRef(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	stdin := strings.NewReader("refs/heads/main " + testSHA + " refs/heads/main " + testSHA + "\n")
	ok, err := g.CheckPush(stdin, "")
	if err != nil {
		t.Fatalf("CheckPush: %v", err)
	}
	if ok {
		t.Error("want block — the pushed sha has no review record")
	}
}

func TestShow_ReportsEachLayerAndTheChain(t *testing.T) {
	g, _ := newTestGate(t, "git@example.com:org/r.git")

	st, err := g.Show(testSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.ChainSatisfied || st.Next != "code-review" {
		t.Errorf("no record: want an unsatisfied chain with code-review next, got %+v", st)
	}
	for _, l := range st.Layers {
		if l.Verdict != "MISSING" {
			t.Errorf("no record: %s = %q, want MISSING", l.Layer, l.Verdict)
		}
	}

	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	st, _ = g.Show(testSHA, "")
	if st.ChainSatisfied || st.Next != "critique" {
		t.Errorf("code-review only: want critique next, got %+v", st)
	}
	if st.Layers[0].Verdict != "ready" || st.Layers[0].By != "cr-1" || st.Layers[0].Ts == "" {
		t.Errorf("code-review layer = %+v, want ready by cr-1 with a timestamp", st.Layers[0])
	}
	if st.Layers[1].Verdict != "MISSING" {
		t.Errorf("critique = %q, want MISSING (the exact state a reviewer probed for)", st.Layers[1].Verdict)
	}

	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	st, _ = g.Show(testSHA, "")
	if !st.ChainSatisfied || st.Next != "" {
		t.Errorf("both ready: want a satisfied chain, got %+v", st)
	}
}

func TestShow_AgreesWithCheckAboutTheChain(t *testing.T) {
	g, _ := newTestGate(t, "git@example.com:org/r.git")
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 3; step++ {
		ok, _ := g.Check(testSHA, "")
		st, _ := g.Show(testSHA, "")
		if ok != st.ChainSatisfied {
			t.Fatalf("step %d: Check ok=%v but Show chain_satisfied=%v", step, ok, st.ChainSatisfied)
		}
		if step == 0 {
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
				t.Fatal(err)
			}
		}
		if step == 1 {
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-2"}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestShow_ShowsSupersededAndNeverWrites(t *testing.T) {
	g, _ := newTestGate(t, "")
	for _, o := range []RecordOpts{
		{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"},
		{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-2"},
	} {
		if _, err := g.Record(o); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(g.recordPath(testSHA))
	st, err := g.Show(testSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Layers[0].Verdict != "changes-needed" || st.Layers[0].SupersededVerdict != "ready" || st.Layers[0].SupersededBy != "cr-1" {
		t.Errorf("superseded trail missing: %+v", st.Layers[0])
	}
	after, _ := os.ReadFile(g.recordPath(testSHA))
	if string(before) != string(after) {
		t.Error("Show changed the record")
	}

	// A sha with no record stays that way: reading must not create the file.
	other := strings.Repeat("b", 40)
	if _, err := g.Show(other, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.recordPath(other)); !os.IsNotExist(err) {
		t.Errorf("Show created a record for an unrecorded sha (stat err = %v)", err)
	}
}

func TestShow_ErrorsInsteadOfFailingOpen(t *testing.T) {
	g, fg := newTestGate(t, "")

	delete(fg.revParse, "HEAD")
	if _, err := g.Show("", ""); err == nil || !strings.Contains(err.Error(), "could not resolve HEAD") {
		t.Errorf("unresolvable HEAD: want an error, got %v (Check would say 'allow' here)", err)
	}
	if _, err := g.Show(testSHA, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a repo path that does not exist must be an error, not an empty answer")
	}
	if err := os.WriteFile(g.recordPath(testSHA), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Show(testSHA, ""); err == nil {
		t.Error("an unparseable record must be an error, not 'MISSING'")
	}
}

func TestShow_ReportsScopeApartFromTheRecord(t *testing.T) {
	g, _ := newTestGate(t, "git@example.com:org/r.git")
	g.ProjectRoot = ""
	g.EnforcedRemotes = []string{"some-other-repo"}
	st, err := g.Show(testSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Scope != "out-of-scope" {
		t.Errorf("scope = %q, want out-of-scope", st.Scope)
	}
	if ok, _ := g.Check(testSHA, ""); !ok {
		t.Error("precondition: Check allows an out-of-scope repo with no record")
	}
	if st.ChainSatisfied || st.Layers[0].Verdict != "MISSING" {
		t.Errorf("Show must still report the (empty) record there, got %+v", st)
	}
}

// A reviewer passes whatever sha it has to hand, often the abbreviated one from a commit message.
// Records are keyed by the FULL sha, so without normalisation Show reports a false MISSING, which is
// exactly the misleading probe result this verb exists to replace.
func TestShow_FindsARecordFromAnAbbreviatedSha(t *testing.T) {
	g, fg := newTestGate(t, "")
	fg.revParse["aaaaaaa"] = testSHA
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	st, err := g.Show("aaaaaaa", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != testSHA {
		t.Errorf("SHA = %q, want the resolved full %q", st.SHA, testSHA)
	}
	if st.Layers[0].Verdict != "ready" {
		t.Errorf("code-review = %q, want ready: an abbreviated sha must find the full-sha record", st.Layers[0].Verdict)
	}
}

// --- the reviewed base ---------------------------------------------------------------

const testBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func baseGate(t *testing.T) *Gate {
	t.Helper()
	g, fg := newTestGate(t, "")
	fg.revParse["origin/main^{commit}"] = testBase
	fg.revParse["origin/other^{commit}"] = "cccccccccccccccccccccccccccccccccccccccc"
	g.Git = fg
	return g
}

func layerOf(t *testing.T, g *Gate, layer string) Layer {
	t.Helper()
	rec, err := g.ReadRecord(testSHA)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Layers[layer]
}

// legacyReRecord writes the record a gate that did not re-check independence could have written:
// the code-review layer re-recorded ready under by. Record no longer produces it (see
// TestRecord_ReadyCodeReviewUnderTheCritiqueRoleIsRefused), but such records exist on disk, and
// the amend path must still refuse to widen them.
func legacyReRecord(t *testing.T, g *Gate, by string) {
	t.Helper()
	rec, err := g.ReadRecord(testSHA)
	if err != nil {
		t.Fatal(err)
	}
	l := rec.Layers["code-review"]
	l.By = by
	rec.Layers["code-review"] = l
	if err := g.writeRecord(testSHA, rec); err != nil {
		t.Fatal(err)
	}
}

// A record written before the field, or a layer recorded without --base, has NO base: it covers only its own
// sha. This is the strict default the lineage check relies on, so it is pinned at the record.
func TestRecord_NoBaseIsRecordedAsNoBase(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if b := layerOf(t, g, "code-review").Base; b != "" {
		t.Errorf("a layer recorded without --base must carry none, got %q", b)
	}
	// and an old on-disk record (no "base" key at all) reads as no base
	raw, _ := os.ReadFile(g.recordPath(testSHA))
	if strings.Contains(string(raw), `"base"`) {
		t.Errorf("an absent base must not be written: %s", raw)
	}
}

func TestRecord_BaseIsStoredResolvedOnTheLayer(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Base: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	if b := layerOf(t, g, "code-review").Base; b != testBase {
		t.Errorf("the base is stored as the resolved sha, got %q", b)
	}
	// the base is per LAYER: critique recorded without one covers only its sha
	if b := layerOf(t, g, "critique").Base; b != "" {
		t.Errorf("critique did not record a base and must not inherit code-review's: %q", b)
	}
	st, err := g.Show(testSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Layers[0].Base != testBase || st.Layers[1].Base != "" {
		t.Errorf("show reports each layer's own base: %+v", st.Layers)
	}
}

func TestRecord_BaseRefusals(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Base: "no-such-ref"}); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("an unresolvable base is refused, not stored: %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-1", Base: "origin/main"}); err == nil || !strings.Contains(err.Error(), "ready verdict only") {
		t.Errorf("a base qualifies a ready only: %v", err)
	}
	if _, err := os.Stat(g.recordPath(testSHA)); err == nil {
		t.Error("a refused record must write nothing")
	}
}

// Records stay write-once: adding a base to a ready layer that has none is the documented amend (the same
// verdict again, re-stamped), re-recording without one keeps it, and a DIFFERENT base is refused.
func TestRecord_BaseIsWriteOnceAndAddableToABaselessReady(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2", Base: "origin/main"}); err != nil {
		t.Fatalf("adding a base to a baseless ready is the amend path: %v", err)
	}
	l := layerOf(t, g, "code-review")
	if l.Base != testBase || l.By != "cr-1" {
		t.Errorf("the amend keeps the original reviewer's attribution: %+v", l)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if b := layerOf(t, g, "code-review").Base; b != testBase {
		t.Errorf("re-recording without --base keeps the base: %q", b)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2", Base: "origin/other"}); err == nil || !strings.Contains(err.Error(), "not rewritten") {
		t.Errorf("a different base must be refused: %v", err)
	}
	if b := layerOf(t, g, "code-review").Base; b != testBase {
		t.Errorf("the refusal left the base alone: %q", b)
	}
	// a dissent carries no coverage
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "cr-3"}); err != nil {
		t.Fatal(err)
	}
	if b := layerOf(t, g, "code-review").Base; b != "" {
		t.Errorf("a changes-needed supersede drops the base: %q", b)
	}
}

// An amend widens what a layer covers, which the lineage check trusts, so only the role that made the
// verdict may do it. The critique role widening the code-review layer would leave both layers standing
// under one role.
func TestRecord_AmendingABaseIsRefusedAcrossRoles(t *testing.T) {
	cases := []struct {
		name, layer, existingBy, amender, want string
	}{
		{"critique role widens code-review", "code-review", "cr-1", "crit-2", "different role"},
		{"code-review role widens critique", "critique", "crit-1", "cr-2", "different role"},
		{"an unrelated name widens code-review", "code-review", "cr-1", "claude", "different role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := baseGate(t)
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
				t.Fatal(err)
			}
			before := layerOf(t, g, tc.layer)
			_, err := g.Record(RecordOpts{SHA: testSHA, Layer: tc.layer, Verdict: "ready", By: tc.amender, Base: "origin/main"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the amend must be refused (%q): %v", tc.want, err)
			}
			if after := layerOf(t, g, tc.layer); after != before {
				t.Errorf("a refused amend left the layer untouched: before %+v after %+v", before, after)
			}
		})
	}
}

// The same-role rule alone is not enough: a code-review layer that already stands under the critique role
// (a legacy record: the gate no longer accepts the re-record that made it) must not be widenable by that role either.
func TestRecord_AmendingACodeReviewBaseIsRefusedToTheCritiqueRole(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	// A legacy record: code-review standing under a critique name, carrying no base.
	legacyReRecord(t, g, "crit-2")
	_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "crit-3", Base: "origin/main"})
	if err == nil || !strings.Contains(err.Error(), "critique-role") {
		t.Fatalf("the critique role must not widen the code-review layer: %v", err)
	}
	if b := layerOf(t, g, "code-review").Base; b != "" {
		t.Errorf("a refused amend wrote a base: %q", b)
	}
}

// The independence re-check is not specific to the critique role's name: a code-review layer that stands
// under the same role as the critique layer is refused a widening from that role too.
func TestRecord_AmendingACodeReviewBaseIsRefusedToTheRoleThatRecordedCritique(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "auditor-1"}); err != nil {
		t.Fatal(err)
	}
	legacyReRecord(t, g, "auditor-2")
	_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "auditor-3", Base: "origin/main"})
	if err == nil || !strings.Contains(err.Error(), "same role") {
		t.Fatalf("the role that recorded critique must not widen code-review: %v", err)
	}
	if b := layerOf(t, g, "code-review").Base; b != "" {
		t.Errorf("a refused amend wrote a base: %q", b)
	}
}

// The role that made the verdict can still widen it: a forgotten --base is recoverable on the same sha,
// by any instance of the same role, for either layer, and the layer keeps its original By.
func TestRecord_AmendingABaseIsAllowedToTheSameRole(t *testing.T) {
	for _, layer := range []string{"code-review", "critique"} {
		t.Run(layer, func(t *testing.T) {
			g := baseGate(t)
			other := "cr-2"
			if layer == "critique" {
				other = "crit-2"
			}
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: layer, Verdict: "ready", By: other, Base: "origin/main"}); err != nil {
				t.Fatalf("same-role amend: %v", err)
			}
			if l := layerOf(t, g, layer); l.Base != testBase || l.By == other {
				t.Errorf("amend not recorded or re-attributed: %+v", l)
			}
		})
	}
}

// A dissent on one layer demotes the sibling ready, which must take its base with it.
func TestRecord_DemotedSiblingLosesItsBase(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Base: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "changes-needed", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	if l := layerOf(t, g, "code-review"); l.Verdict != supersededMarker || l.Base != "" {
		t.Errorf("a demoted ready covers nothing: %+v", l)
	}
}

// A base that resolves to a non-commit object is refused at record time, not stored to fail every lineage check.
func TestRecord_RefusesABaseThatIsNotACommit(t *testing.T) {
	dir := t.TempDir()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q")
	run("", "commit", "-q", "--allow-empty", "-m", "c1")
	head := run("", "rev-parse", "HEAD")
	blob := run("x", "hash-object", "-w", "--stdin")
	g := &Gate{RecordsDir: t.TempDir(), Git: execGit{}, ProjectRoot: "/project-root"}

	_, err := g.Record(RecordOpts{SHA: head, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: dir, Base: blob})
	if err == nil || !strings.Contains(err.Error(), "does not resolve to a commit") {
		t.Fatalf("a blob base must be refused: %v", err)
	}
	if _, err := os.Stat(g.recordPath(head)); err == nil {
		t.Error("a refused record must write nothing")
	}
	for _, junk := range []string{"--foo", "-h", "--end-of-options"} {
		_, err = g.Record(RecordOpts{SHA: head, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: dir, Base: junk})
		if err == nil || !strings.Contains(err.Error(), "does not resolve to a commit") {
			t.Errorf("a dash-prefixed base %q must be refused: %v", junk, err)
		}
		if _, serr := os.Stat(g.recordPath(head)); serr == nil {
			t.Fatalf("a refused base %q must write no record", junk)
		}
	}
	if _, err := g.Record(RecordOpts{SHA: head, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: dir, Base: head}); err != nil {
		t.Fatalf("a commit base is accepted: %v", err)
	}
}

// A record written before the base field has no "base" key and reads as covering only its own sha.
func TestReadRecord_OldRecordWithoutBaseKey(t *testing.T) {
	g, _ := newTestGate(t, "")
	old := `{"sha":"` + testSHA + `","layers":{"code-review":{"verdict":"ready","by":"cr-1","ts":"2025-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(g.recordPath(testSHA), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := layerOf(t, g, "code-review"); l.Verdict != "ready" || l.By != "cr-1" || l.Base != "" {
		t.Errorf("old record must read as ready with no base: %+v", l)
	}
	st, err := g.Show(testSHA, "")
	if err != nil || st.Layers[0].Base != "" {
		t.Errorf("show of an old record: %+v %v", st, err)
	}
}
