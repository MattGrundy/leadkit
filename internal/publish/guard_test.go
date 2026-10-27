package publish

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPermitted_RefusesEveryWayToCreateOrSendARepository pins the standing order in code: this
// tool prepares and verifies, and a human does the rest (tickets 198, 267, 276). The permitted
// list is the control for it: a guard that refused everything would pass the refusals alone.
func TestPermitted_RefusesEveryWayToCreateOrSendARepository(t *testing.T) {
	refused := [][]string{
		{"git", "push", "origin", "main"},
		{"git", "-C", "/x", "push"},
		{"git", "-c", "user.name=a", "push"},
		{"git", "remote", "add", "origin", "git@example.test:o/r.git"},
		{"git", "fetch"},
		{"git", "pull"},
		{"git", "clone", "/x", "/y"},
		{"git", "-c", "alias.go=push", "go"},
		{"git", "-c", "core.sshCommand=evil", "log"},
		{"git", "-c", "url.x.insteadOf=y", "log"},
		{"git", "config", "remote.origin.url", "x"},
		{"git", "archive", "--remote=git@example.test:o/r.git", "HEAD"},
		{"git", "ls-tree", "-r", "-z", "HEAD"},
		{"git", "ls-tree", "-r", strings.Repeat("a", 40)},
		{"git", "ls-tree", "-r", "--name-only", strings.Repeat("a", 40)},
		{"git", "ls-tree", "-r", "-z", strings.Repeat("a", 40), "--", "x"},
		{"git", "ls-tree", "-z", "-r", strings.Repeat("a", 40)},
		{"git", "cat-file", "-p", "HEAD"},
		{"git", "cat-file", "--batch-all-objects"},
		{"git", "cat-file", "--batch", "--follow-symlinks"},
		{"git", "cat-file", "--batch-command"},
		{"git", "cat-file"},
		{"git", "for-each-ref"},
		{"git", "for-each-ref", "--format=%(refname)"},
		{"git", "for-each-ref", "--format=" + refFormat, "refs/tags"},
		{"git", "for-each-ref", "--count=1", "--format=" + refFormat},
		{"git", "for-each-ref", "--contains", "HEAD", "--format=" + refFormat},
		{"git", "for-each-ref", "--format=" + refFormat + "x"},
		{"git", "reset", "--hard", "origin/main"},
		{"git", "reset", "-q", "--hard", "HEAD"},
		{"git", "clean", "-fdx"},
		{"git", "-C"},
		{"git"},
		{"gh", "repo", "create", "o/r"},
		{"gh", "repo", "delete", "o/r"},
		{"gh", "repo", "fork"},
		{"gh", "repo", "rename", "x"},
		{"gh", "repo", "edit", "--visibility", "public"},
		{"gh", "api", "-X", "POST", "user/repos"},
		{"gh", "variable", "set", "X", "--body", "y"},
		{"gh", "variable", "delete", "X"},
		{"gh", "variable", "get", "X", "--repo", "o/r", "--extra"},
		{"glab", "repo", "create"},
		{"sh", "-c", "git push"},
		{},
	}
	for _, argv := range refused {
		if err := Permitted(argv); err == nil {
			t.Errorf("Permitted(%v) = nil, want a refusal", argv)
		} else if !strings.Contains(err.Error(), "never creates") {
			t.Errorf("Permitted(%v) refusal must say why: %v", argv, err)
		}
	}
	sha := strings.Repeat("a", 40)
	for _, argv := range [][]string{
		{"git", "-C", "/x", "rev-parse", "--verify", "-q", "HEAD"},
		{"git", "-C", "/x", "archive", "--format=tar", sha},
		{"git", "-C", "/x", "init", "-q", "-b", "main", "--template="},
		{"git", "-C", "/x", "-c", "core.excludesFile=/dev/null", "add", "-A"},
		{"git", "-C", "/x", "-c", "user.name=n", "-c", "user.email=e", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "m"},
		{"git", "-C", "/x", "log", "--all"},
		{"git", "-C", "/x", "rev-list", "--count", "--all"},
		{"git", "-C", "/x", "ls-files", "-z"},
		{"git", "-C", "/x", "status", "--porcelain"},
		{"git", "-C", "/x", "for-each-ref", "--format=" + refFormat},
		{"git", "-C", "/x", "ls-tree", "-r", "-z", sha},
		{"git", "-C", "/x", "cat-file", "--batch"},
		{"git", "-C", "/x", "reset", "-q", "--hard", sha},
		{"git", "-C", "/x", "clean", "-fdq"},
		{"gh", "variable", "get", "LEADKIT_SCRUB_TERMS"},
		{"gh", "variable", "get", "LEADKIT_SCRUB_TERMS", "--repo", "o/r"},
	} {
		if err := Permitted(argv); err != nil {
			t.Errorf("Permitted(%v) = %v, want nil: the tool needs it", argv, err)
		}
	}
}

func TestGuard_StopsACommandBeforeItReachesTheRunner(t *testing.T) {
	inner := &recorder{}
	g := Guard(inner)
	if _, _, err := g.Run([]string{"git", "push", "origin", "main"}, nil); err == nil {
		t.Fatal("want a refusal")
	}
	if len(inner.calls) != 0 {
		t.Fatalf("a refused command must never reach the executor: %v", inner.calls)
	}
}

// TestNoCodePathBuildsAForbiddenCommand reads the package's own source: an allow-list is only as
// good as the argv the package assembles, and this is what keeps a later edit honest about it.
func TestNoCodePathBuildsAForbiddenCommand(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") || e.Name() == "guard.go" {
			continue
		}
		data, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, verb := range []string{`"push"`, `"remote"`, `"fetch"`, `"clone"`, `"pull"`, `"create"`} {
			if bytes.Contains(data, []byte(verb)) {
				t.Errorf("%s names the git/gh verb %s; this package must not build that command", e.Name(), verb)
			}
		}
	}
}

func TestParseManifest(t *testing.T) {
	got, err := ParseManifest("// c\n\n docs/a.md \ndocs/dir/\n./docs/a.md\nONBOARDING.md\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "docs/a.md,docs/dir,ONBOARDING.md"; strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s (cleaned, de-duplicated, comments dropped)", got, want)
	}
	// The comment marker is the scrub term file's: a "#" line is a path, not a comment.
	if got, _ := ParseManifest("#notes\n"); len(got) != 1 {
		t.Errorf("only // starts a comment, as in the term file: %v", got)
	}
}

func tarOf(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		h := h
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len("x"))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("x"))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTar_RefusesEntriesThatEscape(t *testing.T) {
	for name, data := range map[string][]byte{
		"parent traversal": tarOf(t, tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}),
		"absolute":         tarOf(t, tar.Header{Name: "/tmp/evil", Typeflag: tar.TypeReg, Mode: 0o644}),
		"through a symlink": tarOf(t,
			tar.Header{Name: "lnk", Typeflag: tar.TypeSymlink, Linkname: "/tmp"},
			tar.Header{Name: "lnk/evil", Typeflag: tar.TypeReg, Mode: 0o644}),
		"a device": tarOf(t, tar.Header{Name: "dev", Typeflag: tar.TypeChar}),
	} {
		dest := t.TempDir()
		if err := extractTar(data, dest); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	dest := t.TempDir()
	ok := tarOf(t, tar.Header{Name: "a/b.txt", Typeflag: tar.TypeReg, Mode: 0o755}, tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "a/b.txt"})
	if err := extractTar(ok, dest); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "a", "b.txt")); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("a regular entry must extract with its executable bit: %v %v", fi, err)
	}
	if _, err := os.Readlink(filepath.Join(dest, "l")); err != nil {
		t.Errorf("a symlink must extract as a symlink: %v", err)
	}
	if err := extractTar([]byte("not a tar"), t.TempDir()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("garbage must be an error: %v", err)
	}
}

// TestPermitted_ArchiveRemoteIsRefusedInEverySpelling defends the standing-order guard against
// contacting another repository (ticket 330, pin A1). Mutant it kills: guard.go's
// strings.HasPrefix(x, "--remote") narrowed to "--remote=" , which leaves the existing test
// (the "=" form) green while `git archive --remote <url>` is permitted; that space form does
// contact the remote. The permitted control keeps the guard from passing by refusing everything.
func TestPermitted_ArchiveRemoteIsRefusedInEverySpelling(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, argv := range [][]string{
		{"git", "archive", "--remote", "git@example.test:o/r.git", "HEAD"},
		{"git", "-C", "/x", "archive", "--remote", "git@example.test:o/r.git", sha},
		{"git", "archive", "--format=tar", "--remote", "/elsewhere", sha},
		{"git", "archive", sha, "--remote", "git@example.test:o/r.git"},
		{"git", "archive", "--remote=git@example.test:o/r.git", sha},
	} {
		err := Permitted(argv)
		if err == nil {
			t.Errorf("Permitted(%v) = nil: archive --remote contacts another repository", argv)
		} else if !strings.Contains(err.Error(), "contact another repository") {
			// refuse() echoes the whole argv, so the reason has to be matched on the guard's own words.
			t.Errorf("Permitted(%v) must refuse for the remote, not for some other reason: %v", argv, err)
		}
	}
	if err := Permitted([]string{"git", "-C", "/x", "archive", "--format=tar", sha}); err != nil {
		t.Errorf("control: a local archive is what the tool needs: %v", err)
	}
}

// TestPermitted_TheNarrowFormsAdmitNothingBeyondTheirOneSpelling pins the edges of the argument
// checks that the main list leaves loose (ticket 330, pins A2 to A6). Each entry names the mutant
// that would let it through:
//   - reset trailing args: len(a) == 3 relaxed to len(a) >= 3
//   - the sha40 end anchor: `$` dropped from the pattern (and the same for the start anchor)
//   - gh variable get: len(argv) >= 4 relaxed to >= 3 (it then panics on argv[4:]), argv[2] == "get" dropped,
//     or the --repo pair loosened to any two words
//   - clean: "-fdq" relaxed to a prefix or any -f flag set such as -fdxq
func TestPermitted_TheNarrowFormsAdmitNothingBeyondTheirOneSpelling(t *testing.T) {
	sha := strings.Repeat("a", 40)
	refused := [][]string{
		{"git", "reset", "-q", "--hard", sha, "--"},
		{"git", "reset", "-q", "--hard", sha, "extra"},
		{"git", "reset", "-q", "--hard", sha + "0"},
		{"git", "reset", "-q", "--hard", sha + "\n"},
		{"git", "reset", "-q", "--hard", "0" + sha},
		{"git", "reset", "-q", "--hard", strings.Repeat("a", 39)},
		{"git", "reset", "-q", "--hard", strings.ToUpper(sha)},
		{"git", "ls-tree", "-r", "-z", sha + "0"},
		{"git", "ls-tree", "-r", "-z", "0" + sha},
		{"gh", "variable", "get"},
		{"gh", "variable", "get", "--repo", "o/r"},
		{"gh", "variable", "get", "X", "--repo"},
		{"gh", "variable", "get", "X", "o/r"},
		{"gh", "variable", "get", "X", "--org", "o"},
		{"gh", "variable", "get", "X", "--repo", "o/r", "--env", "prod"},
		{"gh", "variable", "list"},
		{"gh", "variable", "set", "X"},
		{"gh", "variable", "delete", "X"},
		{"gh", "variable"},
		{"gh", "get", "variable", "X"},
		{"git", "clean", "-fdxq"},
		{"git", "clean", "-fdxf"},
		{"git", "clean", "-fd"},
		{"git", "clean", "-fdq", "-x"},
		{"git", "clean"},
	}
	for _, argv := range refused {
		if err := Permitted(argv); err == nil {
			t.Errorf("Permitted(%v) = nil, want a refusal", argv)
		}
	}
}
