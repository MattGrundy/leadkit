package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExternalConfigsNamingRepoByRemote is the check that runs for a repo with no other sign of
// leadkit, i.e. on every push on the machine. It finds a config that names the repo by a remote,
// and nothing else: not the clone directory's name, not a file it cannot read or parse, not a
// directory it cannot list. Each row is one thing it must find or must leave alone.
func TestExternalConfigsNamingRepoByRemote(t *testing.T) {
	remotes := map[string][]string{}
	orig := gitRemoteURLsDir
	gitRemoteURLsDir = func(dir string) []string { return remotes[dir] }
	t.Cleanup(func() { gitRemoteURLsDir = orig })

	cases := []struct {
		name    string
		remotes []string
		repoDir string
		files   map[string]string
		want    string // substring of the one finding; "" = none
	}{
		{"named for a renamed remote's repository", []string{"https://github.com/acme/proj.git"}, "clone", map[string]string{"proj.yaml": "project_id: x\n"}, "proj.yaml is named for this repo"},
		{"named for a secondary remote", []string{"git@github.com:acme/first.git", "git@github.com:acme/second.git"}, "clone", map[string]string{"second.yaml": "project_id: x\n"}, "second.yaml is named for this repo"},
		{"project_origin matches a remote", []string{"https://github.com/acme/proj.git"}, "clone", map[string]string{"legacy.yaml": "project_origin: acme/proj\n"}, "declares project_origin"},
		{"project_id is a remote's repository name", []string{"https://github.com/acme/proj.git"}, "clone", map[string]string{"legacy.yaml": "project_id: Proj\n"}, "declares project_id"},
		{"the clone directory's name is not a handle", []string{"https://github.com/acme/widgets.git"}, "api", map[string]string{"api.yaml": "project_id: api\nproject_origin: other/api\n"}, ""},
		{"a clone directory with no remote at all", nil, "proj", map[string]string{"proj.yaml": "project_id: x\n"}, ""},
		{"a file that cannot be parsed is not evidence", []string{"https://github.com/acme/widgets.git"}, "clone", map[string]string{"broken.yaml": "project_id: [unclosed\n"}, ""},
		{"another repo's config", []string{"https://github.com/acme/widgets.git"}, "clone", map[string]string{"other.yaml": "project_id: other\nproject_origin: acme/other\n"}, ""},
		{"a same-named repo of another owner", []string{"https://github.com/acme/widgets.git"}, "clone", map[string]string{"legacy.yaml": "project_origin: mallory/widgets\n"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			repo := filepath.Join(t.TempDir(), tc.repoDir)
			remotes[repo] = tc.remotes
			got := ExternalConfigsNamingRepoByRemote(dir, repo)
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("want no finding, got %v", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Errorf("want one finding containing %q, got %v", tc.want, got)
			}
		})
	}
}

// A directory that cannot be listed is not a finding for this check (it is for the broad one, which
// is asked only about a repo that is evidently a leadkit project).
func TestExternalConfigsNamingRepoByRemote_UnlistableDirectoryIsSilent(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	if got := ExternalConfigsNamingRepoByRemote(absent, t.TempDir()); len(got) != 0 {
		t.Errorf("got %v", got)
	}
	if got := ExternalConfigsNamingRepo(absent, t.TempDir()); len(got) != 1 {
		t.Errorf("the broad check still reports it, got %v", got)
	}
}
