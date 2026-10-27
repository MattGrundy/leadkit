package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/forge"
)

type publishGH struct{ calls [][]string }

func (g *publishGH) Run(argv []string, _ []byte) ([]byte, []byte, error) {
	g.calls = append(g.calls, argv)
	return nil, []byte("no such variable"), os.ErrNotExist
}

func publishFixture(t *testing.T, leak bool) (repo, terms string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("LEADKIT_SCRUB_TERMS", "") // the ambient term sources must not answer for a test
	t.Setenv("LEADKIT_SCRUB_TERMS_TEXT", "")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		os.Unsetenv(k)
	}
	repo = t.TempDir()
	body := "echo hello\n"
	if leak {
		body = "echo zzcorp\n"
	}
	files := map[string]string{
		"build.sh":                 body,
		"docs/publish-exclude.txt": "docs/internal.md\n",
		"docs/internal.md":         "zzcorp\n",
	}
	for p, c := range files {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"-c", "user.name=a", "-c", "user.email=a@example.test", "commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	terms = filepath.Join(t.TempDir(), "terms.txt")
	if err := os.WriteFile(terms, []byte("zzcorp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, terms
}

func runPublishCmd(args []string) (code int, stdout, stderr string, gh *publishGH) {
	gh = &publishGH{}
	var out, errb bytes.Buffer
	code = publishCmd(args, publishDeps{git: forge.ExecRunner{Timeout: time.Minute}, gh: gh, now: time.Now}, &out, &errb)
	return code, out.String(), errb.String(), gh
}

func TestPublish_Usage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"check", "extra"}, {"check", "--no-such-flag"}} {
		if code, _, _, _ := runPublishCmd(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	// prepare-only flags are not accepted by check.
	if code, _, _, _ := runPublishCmd([]string{"check", "--out", "x"}); code != 2 {
		t.Errorf("check --out: exit %d, want 2", code)
	}
}

// TestPublish_HasNoFlagThatCreatesOrSendsARepository pins the whole flag surface. A new flag
// fails this until it is listed here, which is the moment to ask whether it lifts the standing
// order that publishing is owner-direct (tickets 198, 267, 276).
func TestPublish_HasNoFlagThatCreatesOrSendsARepository(t *testing.T) {
	flagRe := regexp.MustCompile(`(?m)^\s+-([a-z-]+)`)
	want := map[string][]string{
		"check":   {"manifest", "ref", "repo", "require-clean-history", "terms-file", "terms-repo"},
		"prepare": {"author-email", "author-name", "base", "manifest", "message", "out", "ref", "repo", "require-clean-history", "terms-file", "terms-repo"},
	}
	for verb, flags := range want {
		_, _, usage, _ := runPublishCmd([]string{verb, "-h"})
		var got []string
		for _, m := range flagRe.FindAllStringSubmatch(usage, -1) {
			got = append(got, m[1])
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(flags, ",") {
			t.Errorf("publish %s flags = %v, want %v", verb, got, flags)
		}
	}
	for _, bad := range []string{"--push", "--create", "--create-repo", "--remote", "--force", "--apply", "--confirm", "--publish", "--visibility"} {
		if code, _, _, _ := runPublishCmd([]string{"prepare", bad}); code != 2 {
			t.Errorf("prepare %s: exit %d, want 2 (unknown flag)", bad, code)
		}
	}
}

func TestPublish_CheckExitCodes(t *testing.T) {
	repo, terms := publishFixture(t, false)
	code, out, errs, _ := runPublishCmd([]string{"check", "--repo", repo, "--terms-file", terms})
	if code != 0 || !strings.Contains(out, "tree: CLEAN") {
		t.Fatalf("clean: exit %d\n%s%s", code, out, errs)
	}

	repo, terms = publishFixture(t, true)
	code, out, _, _ = runPublishCmd([]string{"check", "--repo", repo, "--terms-file", terms})
	if code != 1 || !strings.Contains(out, "build.sh:1") || !strings.Contains(out, "NOT READY") {
		t.Fatalf("leak: exit %d\n%s", code, out)
	}
}

func TestPublish_UnresolvedTermsRefuseWithExit1AndNoReport(t *testing.T) {
	repo, _ := publishFixture(t, false)
	code, out, errs, gh := runPublishCmd([]string{"check", "--repo", repo, "--terms-repo", "o/r"})
	if code != 1 || !strings.Contains(errs, "REFUSED") || !strings.Contains(errs, "does not resolve") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if strings.Contains(out, "CLEAN") || strings.Contains(out, "CHECKED") {
		t.Errorf("a refusal must not print anything that reads as a pass:\n%s", out)
	}
	if len(gh.calls) != 1 || gh.calls[0][len(gh.calls[0])-1] != "o/r" {
		t.Errorf("the GitHub variable must be consulted against --terms-repo: %v", gh.calls)
	}
}

func TestPublish_PrepareBuildsAndSaysItDidNotPublish(t *testing.T) {
	repo, terms := publishFixture(t, false)
	out := filepath.Join(t.TempDir(), "public")
	code, stdout, errs, _ := runPublishCmd([]string{"prepare", "--repo", repo, "--terms-file", terms, "--out", out})
	if code != 0 || !strings.Contains(stdout, "PREPARED, NOT PUBLISHED") {
		t.Fatalf("exit %d\n%s%s", code, stdout, errs)
	}
	if _, err := os.Stat(filepath.Join(out, "build.sh")); err != nil {
		t.Errorf("the tree must be built: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "docs", "internal.md")); err == nil {
		t.Error("the excluded file must not be in the built tree")
	}
	if code, _, errs, _ := runPublishCmd([]string{"prepare", "--repo", repo, "--terms-file", terms}); code != 1 || !strings.Contains(errs, "exactly one of") {
		t.Errorf("prepare with no target: exit %d %s", code, errs)
	}
}

// A --terms-file that does not exist is a typo, and checking a different list instead would exit 0
// against terms the operator did not name.
func TestPublish_ANamedTermsFileThatDoesNotExistRefuses(t *testing.T) {
	repo, _ := publishFixture(t, false)
	t.Setenv("LEADKIT_SCRUB_TERMS_TEXT", "zzcorp")
	code, out, errs, gh := runPublishCmd([]string{"check", "--repo", repo, "--terms-file", filepath.Join(t.TempDir(), "typo.txt")})
	if code != 1 || !strings.Contains(errs, "does not exist") || out != "" {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if len(gh.calls) != 0 {
		t.Errorf("another source was consulted: %v", gh.calls)
	}
}

func TestPublish_ATermFileWithABOMRefuses(t *testing.T) {
	repo, _ := publishFixture(t, true) // the tree really does leak
	bom := filepath.Join(t.TempDir(), "bom.txt")
	if err := os.WriteFile(bom, []byte("\xef\xbb\xbfzzcorp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs, _ := runPublishCmd([]string{"check", "--repo", repo, "--terms-file", bom})
	if code != 1 || strings.Contains(out, "CLEAN") || !strings.Contains(errs, "U+FEFF") {
		t.Fatalf("a leaking tree and a BOM term file must refuse, not pass: exit %d\n%s%s", code, out, errs)
	}
}
