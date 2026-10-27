package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExternalConfigsNamingRepo reads the config directory for a file that belongs to the repo by
// ANY handle, because the lookup uses one (the origin's name) and a profile is missed when that
// changes. Each row is one handle; the last rows are files that must NOT match.
func TestExternalConfigsNamingRepo(t *testing.T) {
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
		{"named for the origin's repository", []string{"git@github.com:acme/widgets.git"}, "clone", map[string]string{"widgets.yaml": "project_id: x\n"}, "widgets.yaml is named for this repo"},
		{"named for a remote that is not origin", []string{"git@github.com:acme/first.git", "git@github.com:acme/fork-name.git"}, "clone", map[string]string{"fork-name.yaml": "project_id: x\n"}, "fork-name.yaml is named for this repo"},
		{"named for the clone directory", nil, "My Clone", map[string]string{"my-clone.yaml": "project_id: x\n"}, "my-clone.yaml is named for this repo"},
		{"project_origin matches a remote", []string{"https://github.com/acme/widgets.git"}, "clone", map[string]string{"legacy.yaml": "project_origin: acme/widgets\n"}, "declares project_origin"},
		{"project_origin matches a host-qualified remote", []string{"git@github.com:acme/widgets.git"}, "clone", map[string]string{"legacy.yaml": "project_origin: github.com/acme/widgets\n"}, "declares project_origin"},
		{"project_id is this repo's name", []string{"git@github.com:acme/widgets.git"}, "clone", map[string]string{"legacy.yaml": "project_id: Widgets\n"}, "declares project_id"},
		{"a file that cannot be parsed cannot be ruled out", nil, "clone", map[string]string{"broken.yaml": "project_id: [unclosed\n"}, "broken.yaml could not be parsed"},
		{"another repo's config", []string{"git@github.com:acme/widgets.git"}, "clone", map[string]string{"other.yaml": "project_id: other\nproject_origin: acme/other\n"}, ""},
		{"a same-named repo of another owner", []string{"git@github.com:acme/widgets.git"}, "clone", map[string]string{"legacy.yaml": "project_origin: mallory/widgets\n"}, ""},
		{"a non-yaml file and a directory", []string{"git@github.com:acme/widgets.git"}, "clone", map[string]string{"widgets.yaml.bak": "x", "widgets.yml": "x"}, ""},
		{"an empty directory", []string{"git@github.com:acme/widgets.git"}, "clone", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o755); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(t.TempDir(), tc.repoDir)
			remotes[repo] = tc.remotes
			got := ExternalConfigsNamingRepo(dir, repo)
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

// A directory that cannot be read is a finding, not an empty answer: "no file names this repo" is
// only a fact about a directory that was read.
func TestExternalConfigsNamingRepo_UnreadableDirectoryIsAFinding(t *testing.T) {
	got := ExternalConfigsNamingRepo(filepath.Join(t.TempDir(), "absent"), t.TempDir())
	if len(got) != 1 || !strings.Contains(got[0], "could not be listed") {
		t.Errorf("got %v", got)
	}
}
