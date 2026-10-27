package main

// `queue move <from> --dry-run` — a caller omitting <to> while asking for a dry run — must be
// refused. Go's flag parsing stops at the first positional, so "--dry-run" would become <to>
// and the entry would be relinked to a file literally named "--dry-run": gone from its lane,
// exit 0, no warning. The guard lives in the command, not only in the docs, because a caller
// running from inside the queue root passes the root check, so only the guard stops it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// queueFixture builds a minimal store + queue overlay and returns the queue root plus the
// absolute path of the one pending entry.
func queueFixture(t *testing.T) (queueRoot, entry string) {
	t.Helper()
	base := t.TempDir()
	queueRoot = filepath.Join(base, "q")
	store := filepath.Join(base, "store")
	for _, d := range []string{filepath.Join(queueRoot, "lane", "pending"), filepath.Join(queueRoot, "lane", "in-progress"), store} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(store, "1-t.md")
	if err := os.WriteFile(target, []byte("---\nid: 1\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry = filepath.Join(queueRoot, "lane", "pending", "1-t.md")
	if err := os.Symlink(target, entry); err != nil {
		t.Fatal(err)
	}
	return queueRoot, entry
}

func TestQueueMove_RefusesFlagShapedPositional(t *testing.T) {
	bin := testBinary(t)

	// Only <to> is tested, and that is not an oversight. A flag-shaped FIRST positional can
	// never reach the guard: flag parsing either consumes it as a real flag or rejects it as
	// an unknown one, both before the positionals are read. Parsing stops at the first
	// positional, so <to> is the only slot a flag can survive into. The guard still checks
	// both, as cheap defence against a future reordering, but asserting the <from> case would
	// be asserting an unreachable path.
	for _, tc := range []struct{ name, from, to string }{
		{"to is a long flag", "", "--dry-run"},
		{"to is a short flag", "", "-dry-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queueRoot, entry := queueFixture(t)
			from := tc.from
			if from == "" {
				from = entry
			}
			// cwd MUST be inside the queue root, or this test cannot reproduce the bug.
			// The flag-as-destination resolves against the caller's cwd, so from a cwd
			// OUTSIDE the root containment alone would refuse it, so the test could not tell whether
			// the guard exists.
			// The lane dir is where an agent following the docs would be.
			lane := filepath.Join(queueRoot, "lane")
			out, code := runCLIDir(t, bin, lane, nil, "queue", "move", "--queue", queueRoot, from, tc.to)
			// Errorf, not Fatalf: the assertions BELOW are the ones that pin the actual
			// damage, and a Fatalf here would stop the subtest before they ran.
			if code == 0 {
				t.Errorf("want a refusal, got exit 0: %s", out)
			}
			if !strings.Contains(out, "misplaced flag") {
				t.Errorf("refusal should name the cause; got: %s", out)
			}
			// The decisive assertion: the entry must still be where it was. A message is
			// worthless if the move already happened.
			if _, err := os.Lstat(entry); err != nil {
				t.Errorf("the queue entry was moved despite the refusal: %v", err)
			}
			// Glob the LANE too, not just the root: the destination resolves against the
			// caller's cwd, which is the lane, so that is where the stray lands.
			var strays []string
			for _, pat := range []string{
				filepath.Join(queueRoot, "-*"),
				filepath.Join(queueRoot, "*", "-*"),
				filepath.Join(queueRoot, "*", "*", "-*"),
			} {
				found, _ := filepath.Glob(pat)
				strays = append(strays, found...)
			}
			if len(strays) > 0 {
				t.Errorf("a flag-named entry was created inside the queue: %v", strays)
			}
		})
	}
}

// The control: a correct invocation must still work, or the guard above could be passing
// because `queue move` was broken outright.
func TestQueueMove_CorrectInvocationStillMoves(t *testing.T) {
	bin := testBinary(t)
	queueRoot, entry := queueFixture(t)
	dest := filepath.Join(queueRoot, "lane", "in-progress") + string(os.PathSeparator)

	out, code := runCLIDir(t, bin, filepath.Join(queueRoot, "lane"), nil, "queue", "move", "--queue", queueRoot, entry, dest)
	if code != 0 {
		t.Fatalf("correct invocation refused (exit %d): %s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "lane", "in-progress", "1-t.md")); err != nil {
		t.Errorf("entry did not arrive at the destination: %v", err)
	}
	if _, err := os.Lstat(entry); err == nil {
		t.Error("entry is still in the source lane, so it was copied rather than moved")
	}
}
