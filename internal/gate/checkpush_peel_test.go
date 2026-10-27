package gate

import (
	"strings"
	"testing"
)

const (
	tagObject = "cccccccccccccccccccccccccccccccccccccccc"
	treeTag   = "dddddddddddddddddddddddddddddddddddddddd"
)

// git gives the pre-push hook the sha of the ref's OBJECT. For an annotated tag that is the tag object,
// which no reviewer recorded anything against; the commit it points at is what was reviewed.
func TestCheckPushVerdict_AnAnnotatedTagIsJudgedByTheCommitItPointsAt(t *testing.T) {
	g, fg := newTestGate(t, "github.com/org/web-client")
	fg.revParse[tagObject+"^{commit}"] = testSHA
	g.Git = fg
	line := "refs/tags/v1 " + tagObject + " refs/tags/v1 0000000000000000000000000000000000000000\n"

	// Unrecorded: refused, naming the tag object AND the commit, so the remedy can be run against the commit.
	v, err := g.CheckPushVerdict(strings.NewReader(line), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], tagObject+" -> commit "+testSHA) || !strings.Contains(v.Reasons[0], "no review record for "+shortSHA(testSHA)) {
		t.Fatalf("an unrecorded tag must be refused naming the tag object and the commit, got %+v", v)
	}

	// Recorded for the COMMIT: the same tag passes.
	for _, r := range []RecordOpts{
		{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"},
		{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"},
	} {
		if _, err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := g.CheckPushVerdict(strings.NewReader(line), ""); !v.OK {
		t.Fatalf("a tag whose commit is fully recorded must pass, got %+v", v)
	}
}

// A ref that does not peel to a commit has nothing a record could cover: in scope it is judged as it
// stands, finds no record, and is refused; a git failure reads the same way, fail-closed.
func TestCheckPushVerdict_ARefThatIsNotACommitIsRefusedInScope(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	line := "refs/tags/t " + treeTag + " refs/tags/t 0000000000000000000000000000000000000000\n"
	v, err := g.CheckPushVerdict(strings.NewReader(line), "")
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], "no review record for "+shortSHA(treeTag)) || !strings.Contains(v.Reasons[0], treeTag) {
		t.Fatalf("got %+v", v)
	}
}

// ...and out of scope it is not this gate's business, exactly as before.
func TestCheckPushVerdict_ARefThatIsNotACommitIsAllowedOutOfScope(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	g.ProjectRoot = ""
	line := "refs/tags/t " + treeTag + " refs/tags/t 0000000000000000000000000000000000000000\n"
	if v, err := g.CheckPushVerdict(strings.NewReader(line), ""); err != nil || !v.OK || !v.OutOfScope {
		t.Fatalf("got %+v %v", v, err)
	}
}
