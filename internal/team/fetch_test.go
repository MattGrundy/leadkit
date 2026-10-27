package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubGitFetch builds a fake "clone" on disk (a temp dir with some .md files, some
// non-.md files, and a dotdir) and returns a gitFetchFunc that hands it back —
// FetchReference's caller in tests never touches the network.
func stubGitFetch(t *testing.T, resolvedRef string) gitFetchFunc {
	t.Helper()
	clone := t.TempDir()
	mustWrite(t, filepath.Join(clone, "categories", "01-core", "backend-developer.md"), "---\nname: backend-developer\ndescription: builds backend systems\ntools: Read, Write, Edit, Bash\n---\nbody\n")
	mustWrite(t, filepath.Join(clone, "categories", "01-core", "frontend-developer.md"), "---\nname: frontend-developer\ndescription: builds frontend UIs\ntools: Read, Write, Edit\n---\nbody\n")
	mustWrite(t, filepath.Join(clone, "README.md"), "# awesome subagents\nnot an agent def, but still .md\n")
	mustWrite(t, filepath.Join(clone, "tools", "catalog.py"), "print('should never be copied')\n")
	mustWrite(t, filepath.Join(clone, ".github", "workflows", "ci.yml"), "name: ci\n")
	return func(repoURL, ref string) (string, string, error) {
		// Return a COPY's path is unnecessary — FetchReference only reads, and its
		// own defer os.RemoveAll(clonedDir) would delete our fixture out from under
		// a second call in the same test, so hand back a fresh copy each time.
		cp := t.TempDir()
		mustCopyTree(t, clone, cp)
		return cp, resolvedRef, nil
	}
}

func mustCopyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			mustCopyTree(t, s, d)
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, d, string(data))
	}
}

func TestFetchReference_OnlyMDFilesLand(t *testing.T) {
	orig := gitFetch
	gitFetch = stubGitFetch(t, "abc1234resolvedsha")
	t.Cleanup(func() { gitFetch = orig })

	teamDir := t.TempDir()
	res, err := FetchReference(FetchReferenceOptions{TeamDir: teamDir, Name: "voltagent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 3 { // 2 agent defs + README.md — both .md, .py and .yml excluded
		t.Fatalf("Files = %v, want 3 (.md only)", res.Files)
	}
	for _, f := range res.Files {
		if !strings.HasSuffix(f, ".md") {
			t.Errorf("non-.md file leaked through: %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "tools", "catalog.py")); !os.IsNotExist(err) {
		t.Error("catalog.py must never be vendored")
	}
	if _, err := os.Stat(filepath.Join(res.Dir, ".github", "workflows", "ci.yml")); !os.IsNotExist(err) {
		t.Error("dotdir contents must never be vendored")
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "categories", "01-core", "backend-developer.md")); err != nil {
		t.Errorf("backend-developer.md should have been vendored: %v", err)
	}
}

func TestFetchReference_SourceTxtRecordsTheRef(t *testing.T) {
	orig := gitFetch
	gitFetch = stubGitFetch(t, "deadbeef1234")
	t.Cleanup(func() { gitFetch = orig })

	teamDir := t.TempDir()
	res, err := FetchReference(FetchReferenceOptions{
		TeamDir: teamDir, Name: "voltagent", RepoURL: "https://example.com/x/y", Ref: "v1.2.3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResolvedRef != "deadbeef1234" {
		t.Errorf("ResolvedRef = %q, want deadbeef1234", res.ResolvedRef)
	}
	data, err := os.ReadFile(filepath.Join(res.Dir, "SOURCE.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo: https://example.com/x/y", "ref: v1.2.3", "resolved: deadbeef1234", "fetched_at:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("SOURCE.txt missing %q:\n%s", want, data)
		}
	}
}

func TestFetchReference_DefaultsUsedWhenOptionsEmpty(t *testing.T) {
	orig := gitFetch
	var gotRepo, gotRef string
	gitFetch = func(repoURL, ref string) (string, string, error) {
		gotRepo, gotRef = repoURL, ref
		return stubGitFetch(t, "sha")(repoURL, ref)
	}
	t.Cleanup(func() { gitFetch = orig })

	if _, err := FetchReference(FetchReferenceOptions{TeamDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if gotRepo != DefaultReferenceRepo {
		t.Errorf("repo = %q, want default %q", gotRepo, DefaultReferenceRepo)
	}
	if gotRef != DefaultReferenceRef {
		t.Errorf("ref = %q, want default %q", gotRef, DefaultReferenceRef)
	}
}

func TestFetchReference_IsIdempotent_ReplacesPriorSnapshot(t *testing.T) {
	orig := gitFetch
	gitFetch = stubGitFetch(t, "sha1")
	t.Cleanup(func() { gitFetch = orig })

	teamDir := t.TempDir()
	opts := FetchReferenceOptions{TeamDir: teamDir, Name: "voltagent"}
	if _, err := FetchReference(opts); err != nil {
		t.Fatal(err)
	}
	// Plant a stray file that a second fetch should clear away.
	strayPath := filepath.Join(teamDir, "reference", "voltagent", "stray-leftover.md")
	mustWrite(t, strayPath, "leftover\n")

	if _, err := FetchReference(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strayPath); !os.IsNotExist(err) {
		t.Error("a second fetch should replace the snapshot, not merge with stale leftovers")
	}
}

func TestDeriveRepoName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/VoltAgent/awesome-claude-code-subagents":     "awesome-claude-code-subagents",
		"https://github.com/VoltAgent/awesome-claude-code-subagents.git": "awesome-claude-code-subagents",
		"https://github.com/VoltAgent/awesome-claude-code-subagents/":    "awesome-claude-code-subagents",
	}
	for in, want := range cases {
		if got := DeriveRepoName(in); got != want {
			t.Errorf("DeriveRepoName(%q) = %q, want %q", in, got, want)
		}
	}
}
