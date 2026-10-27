package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// evidenceFixture is a profile with its own records_dir and a gate-bound repo, for driving the real
// verbs. record runs `gate record` as an unbound recorder.
func evidenceFixture(t *testing.T) (record func(args ...string) (string, int), show func() string, recordsDir string) {
	t.Helper()
	bin := testBinary(t)
	home := t.TempDir()
	repo := gitRepoNoCommits(t, "git@example.com:org/evidence.git")
	recordsDir = filepath.Join(t.TempDir(), "records")
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nrecords_dir: "+recordsDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record = func(args ...string) (string, int) {
		a := append([]string{"gate", "record", "--repo", repo, "--sha", scopeTestFakeSHA, "--by", "cr-1"}, args...)
		return gateAs(t, bin, home, profile, "", a...)
	}
	show = func() string {
		out, code := gateAs(t, bin, home, profile, "", "gate", "show", "--repo", repo, "--sha", scopeTestFakeSHA)
		if code != 0 {
			t.Fatalf("gate show (exit %d):\n%s", code, out)
		}
		return out
	}
	return record, show, recordsDir
}

func TestCLI_GateRecord_AReadyWithoutEvidenceOrExemptionIsRefused(t *testing.T) {
	record, _, recordsDir := evidenceFixture(t)
	out, code := record("--layer", "code-review", "--verdict", "ready")
	if code != 2 || !strings.Contains(out, "mutation evidence") || !strings.Contains(out, "--no-mutant") {
		t.Fatalf("a bare ready must be refused naming the remedy (exit %d):\n%s", code, out)
	}
	if entries, _ := os.ReadDir(recordsDir); len(entries) != 0 {
		t.Errorf("a refused record wrote %v", entries)
	}
	if out, code := record("--layer", "code-review", "--verdict", "ready", "--no-mutant", "  "); code != 2 {
		t.Errorf("a blank exemption is silence, not an exemption (exit %d):\n%s", code, out)
	}
	// A blocking finding is never gated behind paperwork.
	if out, code := record("--layer", "code-review", "--verdict", "changes-needed"); code != 0 {
		t.Errorf("changes-needed needs no evidence (exit %d):\n%s", code, out)
	}
}

func TestCLI_GateRecord_EvidenceAndExemptionAreStoredAndShown(t *testing.T) {
	record, show, _ := evidenceFixture(t)
	if out, code := record("--layer", "code-review", "--verdict", "ready", "--mutants", "drop live-claim branch -> TestStall_Held"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if out := show(); !strings.Contains(out, "mutation evidence: drop live-claim branch -> TestStall_Held") {
		t.Errorf("gate show lacks the stored evidence:\n%s", out)
	}
	record2, show2, _ := evidenceFixture(t)
	if out, code := record2("--layer", "code-review", "--verdict", "ready", "--no-mutant", "comment-only diff"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if out := show2(); !strings.Contains(out, "mutation evidence: no mutant applies, because comment-only diff") {
		t.Errorf("gate show lacks the stored exemption:\n%s", out)
	}
}

func TestCLI_GateShow_ARecordWithoutTheFieldSaysNotRecorded(t *testing.T) {
	_, show, recordsDir := evidenceFixture(t)
	if err := os.MkdirAll(recordsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"sha":"` + scopeTestFakeSHA + `","layers":{"code-review":{"verdict":"ready","by":"cr-1","ts":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(filepath.Join(recordsDir, scopeTestFakeSHA+".json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := show()
	if !strings.Contains(out, "mutation evidence: not recorded") || strings.Contains(out, "no mutant applies") {
		t.Errorf("an old record must read as 'not recorded', not as an exemption:\n%s", out)
	}
}

func TestCLI_GateVerdict_AReadyWithoutEvidenceOrExemptionIsRefused(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	out, code := gateAs(t, bin, home, filepath.Join(home, "none.yaml"), "", "gate", "verdict",
		"--ticket", "queue/code-review/pending/none.md", "--verdict", "ready", "--by", "cr-1")
	if code != 2 || !strings.Contains(out, "mutation evidence") {
		t.Errorf("gate verdict ready must be refused for want of evidence (exit %d):\n%s", code, out)
	}
}
