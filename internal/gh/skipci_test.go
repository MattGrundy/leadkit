package gh

import "testing"

func TestHasSkipCIMarker(t *testing.T) {
	tests := map[string]bool{
		"fix: bug [skip ci]":                  true,
		"fix: bug [Skip CI]":                  true,
		"fix: bug [skip   ci]":                true,
		"fix: bug [SKIP CI] more text":        true,
		"fix: bug, nothing special here":      false,
		"fix: skip the ci step (no brackets)": false,
	}
	for msg, want := range tests {
		if got := hasSkipCIMarker(msg); got != want {
			t.Errorf("hasSkipCIMarker(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestHasSkipCI_UsesInjectedGH(t *testing.T) {
	orig := runGH
	defer func() { runGH = orig }()

	t.Run("detected", func(t *testing.T) {
		var gotArgs []string
		runGH = func(args ...string) ([]byte, error) {
			gotArgs = args
			return []byte(`{"commit":{"message":"fix: bug [skip ci]"}}`), nil
		}
		found, err := HasSkipCI("org/repo", "deadbeef")
		if err != nil {
			t.Fatalf("HasSkipCI: %v", err)
		}
		if !found {
			t.Error("want found=true")
		}
		wantArgs := []string{"api", "repos/org/repo/commits/deadbeef"}
		if len(gotArgs) != len(wantArgs) || gotArgs[0] != wantArgs[0] || gotArgs[1] != wantArgs[1] {
			t.Errorf("gh args = %v, want %v", gotArgs, wantArgs)
		}
	})

	t.Run("clean", func(t *testing.T) {
		runGH = func(args ...string) ([]byte, error) {
			return []byte(`{"commit":{"message":"fix: a normal bug"}}`), nil
		}
		found, err := HasSkipCI("org/repo", "deadbeef")
		if err != nil {
			t.Fatalf("HasSkipCI: %v", err)
		}
		if found {
			t.Error("want found=false for a clean message")
		}
	})
}

func TestFormatSkipCIMessage(t *testing.T) {
	got := FormatSkipCIMessage("deadbeef0123456789")
	want := "check-skip-ci: commit deadbeef0123 carries [skip ci] — GitHub creates ZERO checks for this push/PR event, not just one workflow."
	if got != want {
		t.Errorf("FormatSkipCIMessage = %q, want %q", got, want)
	}
}
