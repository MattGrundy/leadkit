package gh

import "testing"

func TestStripSkipCI(t *testing.T) {
	tests := map[string]string{
		"Fix bug [skip ci]":                 "Fix bug",
		"Fix bug [Skip CI]":                 "Fix bug",
		"Fix bug [skip   ci]":               "Fix bug",
		"Fix bug [SKIP CI] and more":        "Fix bug and more",
		"no marker here":                    "no marker here",
		"[skip ci] leading":                 " leading", // only leading whitespace before "[" is stripped, not trailing
		"double [skip ci][skip ci] markers": "double markers",
	}
	for in, want := range tests {
		if got := stripSkipCI(in); got != want {
			t.Errorf("stripSkipCI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposeFromView_MultiCommitWithAndWithoutBodies(t *testing.T) {
	v := prView{
		Title:  "Fix the thing",
		Number: 42,
		Commits: []struct {
			MessageHeadline string `json:"messageHeadline"`
			MessageBody     string `json:"messageBody"`
		}{
			{MessageHeadline: "fix: the thing", MessageBody: "details about the fix"},
			{MessageHeadline: "chore: cleanup", MessageBody: ""},
		},
	}
	subject, body := composeFromView(v)

	if subject != "Fix the thing (#42)" {
		t.Errorf("subject = %q, want %q", subject, "Fix the thing (#42)")
	}
	wantBody := "* fix: the thing\n\ndetails about the fix\n\n* chore: cleanup"
	if body != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestComposeFromView_StripsSkipCIFromTitleAndCommitBody(t *testing.T) {
	v := prView{
		Title:  "Fix the thing [skip ci]",
		Number: 7,
		Commits: []struct {
			MessageHeadline string `json:"messageHeadline"`
			MessageBody     string `json:"messageBody"`
		}{
			{MessageHeadline: "fix: the thing", MessageBody: "notes [Skip CI] more notes"},
		},
	}
	subject, body := composeFromView(v)

	if subject != "Fix the thing (#7)" {
		t.Errorf("subject = %q, want %q (skip ci in the title must be stripped)", subject, "Fix the thing (#7)")
	}
	wantBody := "* fix: the thing\n\nnotes more notes"
	if body != wantBody {
		t.Errorf("body = %q, want %q (skip ci in a commit body must be stripped)", body, wantBody)
	}
}

func TestParsePRView(t *testing.T) {
	fixture := `{"title":"t","number":5,"commits":[{"messageHeadline":"h1","messageBody":"b1"}]}`
	v, err := parsePRView([]byte(fixture))
	if err != nil {
		t.Fatalf("parsePRView: %v", err)
	}
	if v.Title != "t" || v.Number != 5 || len(v.Commits) != 1 || v.Commits[0].MessageHeadline != "h1" {
		t.Errorf("parsed = %+v", v)
	}
}

func TestCompose_UsesInjectedGH(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()

	var gotArgs []string
	runGH = func(args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"title":"Fix bug [skip ci]","number":42,"commits":[` +
			`{"messageHeadline":"fix: bug","messageBody":"details here"},` +
			`{"messageHeadline":"chore: cleanup [Skip CI]","messageBody":""}]}`), nil
	}

	subject, body, err := Compose("org/repo", "42")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if subject != "Fix bug (#42)" {
		t.Errorf("subject = %q, want %q", subject, "Fix bug (#42)")
	}
	wantBody := "* fix: bug\n\ndetails here\n\n* chore: cleanup"
	if body != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
	wantArgs := []string{"pr", "view", "42", "-R", "org/repo", "--json", "title,number,commits"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("gh args = %v, want %v", gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Errorf("gh args[%d] = %q, want %q", i, gotArgs[i], wantArgs[i])
		}
	}
}

func TestFormatMergeMessage_ExactDelimiterFormat(t *testing.T) {
	got := FormatMergeMessage("Fix bug (#42)", "* fix: bug\n\ndetails")
	want := "Fix bug (#42)\n---BODY---\n* fix: bug\n\ndetails\n"
	if got != want {
		t.Errorf("FormatMergeMessage = %q, want %q", got, want)
	}
}
