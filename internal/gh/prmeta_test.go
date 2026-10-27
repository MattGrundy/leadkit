package gh

import (
	"fmt"
	"strings"
	"testing"
)

func TestParsePRMeta(t *testing.T) {
	fixture := `{"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","headRefOid":"abcdef1234567890","title":"Fix the thing","baseRefName":"main"}`
	m, err := parsePRMeta([]byte(fixture))
	if err != nil {
		t.Fatalf("parsePRMeta: %v", err)
	}
	if m.State != "OPEN" || m.IsDraft || m.Mergeable != "MERGEABLE" || m.MergeStateStatus != "CLEAN" ||
		m.HeadRefOID != "abcdef1234567890" || m.Title != "Fix the thing" || m.BaseRefName != "main" {
		t.Errorf("parsed = %+v", m)
	}
}

func TestFetchPRMeta_UsesInjectedGH(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()

	var gotArgs []string
	runGH = func(args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"state":"OPEN","isDraft":true,"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","headRefOid":"deadbeef","title":"T"}`), nil
	}
	m, err := FetchPRMeta("org/repo", "42")
	if err != nil {
		t.Fatalf("FetchPRMeta: %v", err)
	}
	if !m.IsDraft || m.Mergeable != "CONFLICTING" || m.MergeStateStatus != "DIRTY" || m.HeadRefOID != "deadbeef" {
		t.Errorf("meta = %+v", m)
	}
	wantArgs := []string{"pr", "view", "42", "-R", "org/repo", "--json", "state,isDraft,mergeable,mergeStateStatus,headRefOid,title,baseRefName,headRefName,url"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("gh args = %v, want %v", gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Errorf("gh args[%d] = %q, want %q", i, gotArgs[i], wantArgs[i])
		}
	}
}

func TestFetchPRMeta_FetchErrorPropagates(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()
	runGH = func(args ...string) ([]byte, error) {
		return nil, fmt.Errorf("gh: not found")
	}
	if _, err := FetchPRMeta("org/repo", "1"); err == nil {
		t.Fatal("want an error when the fetch fails (caller must fail closed)")
	}
}

func TestCIChecksGreen(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()

	t.Run("green on exit 0", func(t *testing.T) {
		var gotArgs []string
		runGH = func(args ...string) ([]byte, error) {
			gotArgs = args
			return []byte("all checks passing\n"), nil
		}
		green, out, err := CIChecksGreen("org/repo", "42")
		if !green || err != nil {
			t.Fatalf("green=%v err=%v, want green,nil", green, err)
		}
		if out == "" {
			t.Error("want the checks output captured")
		}
		wantArgs := []string{"pr", "checks", "42", "-R", "org/repo"}
		if len(gotArgs) != len(wantArgs) {
			t.Fatalf("gh args = %v, want %v", gotArgs, wantArgs)
		}
		for i := range wantArgs {
			if gotArgs[i] != wantArgs[i] {
				t.Errorf("gh args[%d] = %q, want %q", i, gotArgs[i], wantArgs[i])
			}
		}
	})

	t.Run("not green on non-zero exit", func(t *testing.T) {
		runGH = func(args ...string) ([]byte, error) {
			return []byte("some checks failing\n"), fmt.Errorf("exit status 1")
		}
		green, out, _ := CIChecksGreen("org/repo", "42")
		if green {
			t.Error("want green=false on a non-zero exit (fail-closed)")
		}
		if out == "" {
			t.Error("want the diagnostic output captured even on failure")
		}
	})
}

func TestHasMergeSkipCIMarker_BothOrderings(t *testing.T) {
	tests := map[string]bool{
		"fix: bug [skip ci]":           true,
		"fix: bug [ci skip]":           true, // the [ci skip] ordering GitHub also honours
		"fix: bug [Skip CI]":           true,
		"fix: bug [CI Skip]":           true,
		"fix: bug [ci   skip]":         true,
		"chore: dist [skip ci] and up": true,
		"fix: a normal commit":         false,
		"fix: skip the ci (no bracks)": false,
	}
	for msg, want := range tests {
		if got := hasMergeSkipCIMarker(msg); got != want {
			t.Errorf("hasMergeSkipCIMarker(%q) = %v, want %v", msg, got, want)
		}
	}
}

// The #727/#728 guard for the MERGE squash body: a skip-ci directive riding in a
// CONSTITUENT commit (either ordering) must be stripped from the composed body so it can
// never land on the merge commit and suppress post-merge CI.
func TestComposeMerge_StripsBothOrderingsFromBody(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()
	runGH = func(args ...string) ([]byte, error) {
		return []byte(`{"title":"Fix the thing","number":77,"commits":[` +
			`{"messageHeadline":"fix: the thing","messageBody":"details"},` +
			`{"messageHeadline":"chore: manifests [skip ci]","messageBody":"regen [ci skip] output"}]}`), nil
	}
	subject, body, err := ComposeMerge("org/repo", "77")
	if err != nil {
		t.Fatalf("ComposeMerge: %v", err)
	}
	if hasMergeSkipCIMarker(subject) || hasMergeSkipCIMarker(body) {
		t.Fatalf("composed merge subject/body still carries a skip-ci directive:\n subject=%q\n body=%q", subject, body)
	}
	// Both the [skip ci] headline and the [ci skip] body directive must be gone.
	if strings.Contains(body, "[skip ci]") || strings.Contains(body, "[ci skip]") {
		t.Errorf("body = %q, want no skip-ci directive of either ordering", body)
	}
}

func TestHeadCarriesSkipCI_UsesInjectedGH(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()

	t.Run("detects [ci skip] ordering", func(t *testing.T) {
		var gotArgs []string
		runGH = func(args ...string) ([]byte, error) {
			gotArgs = args
			return []byte(`{"commit":{"message":"chore: manifests [ci skip]"}}`), nil
		}
		found, err := HeadCarriesSkipCI("org/repo", "deadbeef")
		if err != nil {
			t.Fatalf("HeadCarriesSkipCI: %v", err)
		}
		if !found {
			t.Error("want found=true for a [ci skip] HEAD")
		}
		wantArgs := []string{"api", "repos/org/repo/commits/deadbeef"}
		if len(gotArgs) != len(wantArgs) || gotArgs[0] != wantArgs[0] || gotArgs[1] != wantArgs[1] {
			t.Errorf("gh args = %v, want %v", gotArgs, wantArgs)
		}
	})

	t.Run("clean HEAD", func(t *testing.T) {
		runGH = func(args ...string) ([]byte, error) {
			return []byte(`{"commit":{"message":"feat: a normal head commit"}}`), nil
		}
		found, err := HeadCarriesSkipCI("org/repo", "deadbeef")
		if err != nil || found {
			t.Errorf("found=%v err=%v, want false,nil", found, err)
		}
	})

	t.Run("fetch error propagates (caller fails closed)", func(t *testing.T) {
		runGH = func(args ...string) ([]byte, error) {
			return nil, fmt.Errorf("gh: api boom")
		}
		if _, err := HeadCarriesSkipCI("org/repo", "deadbeef"); err == nil {
			t.Fatal("want an error when the commit is unreadable")
		}
	})
}

func TestFetchMergeCommit(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()

	var gotArgs []string
	runGH = func(args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"state":"MERGED","mergeCommit":{"oid":"abc123"}}`), nil
	}
	m, err := FetchMergeCommit("org/repo", "42")
	if err != nil || m.State != "MERGED" || m.OID != "abc123" {
		t.Fatalf("FetchMergeCommit = %+v, %v", m, err)
	}
	if want := "pr view 42 -R org/repo --json state,mergeCommit"; strings.Join(gotArgs, " ") != want {
		t.Errorf("gh args = %q, want %q", gotArgs, want)
	}

	// An open PR has a null mergeCommit: no sha, and not an error.
	runGH = func(args ...string) ([]byte, error) { return []byte(`{"state":"OPEN","mergeCommit":null}`), nil }
	if m, err := FetchMergeCommit("org/repo", "42"); err != nil || m.State != "OPEN" || m.OID != "" {
		t.Errorf("open PR: %+v, %v", m, err)
	}
	runGH = func(args ...string) ([]byte, error) { return nil, fmt.Errorf("boom") }
	if _, err := FetchMergeCommit("org/repo", "42"); err == nil {
		t.Error("a failed read must be an error, never an empty sha")
	}
}

func TestCommitMessage(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()
	runGH = func(args ...string) ([]byte, error) {
		return []byte(`{"commit":{"message":"subject\n\nTicket: 265"}}`), nil
	}
	if got, err := CommitMessage("org/repo", "abc"); err != nil || got != "subject\n\nTicket: 265" {
		t.Errorf("CommitMessage = %q, %v", got, err)
	}
}
