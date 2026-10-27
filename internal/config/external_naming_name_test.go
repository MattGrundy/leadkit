package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dominant handle is a file named for the repo, and it is looked up by name, as the lookup does.
// Nothing about the file's contents, its permissions, its spelling's case or the directory's
// listability may hide it. Each test is one way it used to be hidden, or could be.

func nameTestEnv(t *testing.T, remotes ...string) (configDir, repo string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "clone")
	m := map[string][]string{repo: remotes}
	orig := gitRemoteURLsDir
	gitRemoteURLsDir = func(dir string) []string { return m[dir] }
	t.Cleanup(func() { gitRemoteURLsDir = orig })
	return t.TempDir(), repo
}

func notRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
}

// A file named for a remote names the repo whether or not it can be parsed or read: the lookup
// would have used it, and "it is broken" is a reason to refuse, not to look elsewhere.
func TestExternalConfigsNaming_AFileNamedForARemoteRefusesWhateverItsContents(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		mode os.FileMode
	}{
		{"unparseable", "project_id: [unclosed\n", 0o644},
		{"readable and valid", "records_dir: /x\n", 0o644},
		{"mode 000", "records_dir: /x\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode == 0 {
				notRoot(t)
			}
			dir, repo := nameTestEnv(t, "https://github.com/acme/proj.git")
			path := filepath.Join(dir, "proj.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			for name, fn := range map[string]func(string, string) []string{"narrow": ExternalConfigsNamingRepoByRemote, "broad": ExternalConfigsNamingRepo} {
				got := fn(dir, repo)
				if len(got) != 1 || !strings.Contains(got[0], "proj.yaml is named for this repo") {
					t.Errorf("%s: want exactly one finding naming the file, got %v", name, got)
				}
			}
		})
	}
}

// A hand-named Proj.yaml is found by the lookup on a case-insensitive filesystem (a stat of
// proj.yaml succeeds), so it must be found here, whichever case the listing spells it in.
func TestExternalConfigsNaming_ACaseVariantFileIsFoundWhereTheLookupWouldFindIt(t *testing.T) {
	dir, repo := nameTestEnv(t, "https://github.com/acme/proj.git")
	if err := os.WriteFile(filepath.Join(dir, "Proj.yaml"), []byte("records_dir: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "proj.yaml")); err != nil {
		t.Skip("this filesystem is case-sensitive, so the lookup would not find Proj.yaml either")
	}
	for name, fn := range map[string]func(string, string) []string{"narrow": ExternalConfigsNamingRepoByRemote, "broad": ExternalConfigsNamingRepo} {
		if got := fn(dir, repo); len(got) != 1 || !strings.Contains(got[0], "is named for this repo") {
			t.Errorf("%s: want one finding, got %v", name, got)
		}
	}
}

// The name is found without listing the directory: a directory that can be searched but not listed
// (mode 0311) still holds proj.yaml, and a stat of it works.
func TestExternalConfigsNaming_ADirectoryThatCannotBeListedStillYieldsAFileNamedForARemote(t *testing.T) {
	notRoot(t)
	dir, repo := nameTestEnv(t, "https://github.com/acme/proj.git")
	if err := os.WriteFile(filepath.Join(dir, "proj.yaml"), []byte("records_dir: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("this filesystem lists a mode 0311 directory")
	}
	if got := ExternalConfigsNamingRepoByRemote(dir, repo); len(got) != 1 || !strings.Contains(got[0], "proj.yaml is named for this repo") {
		t.Errorf("narrow: want the file named for the remote, got %v", got)
	}
	got := ExternalConfigsNamingRepo(dir, repo)
	if len(got) != 2 || !strings.Contains(got[0], "proj.yaml is named for this repo") || !strings.Contains(got[1], "could not be listed") {
		t.Errorf("broad: want the file and the listing failure, got %v", got)
	}
}

// What an unlistable directory still hides: a config that has only a project_origin or project_id
// handle, which needs the listing and the contents. The lookup could not find it either.
func TestExternalConfigsNaming_AConfigWithOnlyAContentHandleIsInvisibleInAnUnlistableDirectory(t *testing.T) {
	notRoot(t)
	dir, repo := nameTestEnv(t, "https://github.com/acme/proj.git")
	if err := os.WriteFile(filepath.Join(dir, "legacy.yaml"), []byte("project_origin: acme/proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("this filesystem lists a mode 0311 directory")
	}
	if got := ExternalConfigsNamingRepoByRemote(dir, repo); len(got) != 0 {
		t.Errorf("a known limit, pinned so that closing it is deliberate: got %v", got)
	}
}

// A file found by name is reported once, not again when the listing reaches it.
func TestExternalConfigsNaming_AFileFoundByNameIsNotReportedTwice(t *testing.T) {
	dir, repo := nameTestEnv(t, "https://github.com/acme/proj.git")
	if err := os.WriteFile(filepath.Join(dir, "proj.yaml"), []byte("project_id: proj\nproject_origin: acme/proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ExternalConfigsNamingRepoByRemote(dir, repo); len(got) != 1 {
		t.Errorf("want one finding, got %v", got)
	}
}
