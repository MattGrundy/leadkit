package main

import (
	"path/filepath"
	"testing"
)

// Ticket 319 runs the naming check for repos with no .leadkit/ directory, which means on every push
// on the machine. So it may only refuse on a definite match. These are the things that must NOT
// refuse such a repo: a broken config for some other project, a config that happens to be named like
// the clone directory, a machine with no config directory at all.
func TestNamingNoise_ARepoWithNoSignOfLeadkitIsNotRefusedForUnrelatedConfigs(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	plain := func(t *testing.T, dirName, origin string) string {
		repo := filepath.Join(t.TempDir(), dirName)
		initRepoWithOrigin(t, repo, origin)
		return repo
	}
	allowed := func(t *testing.T, home, repo string) {
		t.Helper()
		deny, report, code := hookRun(t, bin, home, repo, "git push")
		if deny != "" || report != "" || code != 0 {
			t.Fatalf("a repo with no sign of leadkit must be a silent allow; deny=%q report=%q exit=%d", deny, report, code)
		}
	}

	t.Run("a config for another project that cannot be parsed", func(t *testing.T) {
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "broken.yaml"), "project_id: [unclosed\n")
		allowed(t, home, plain(t, "widgets", "https://github.com/acme/widgets.git"))
	})

	t.Run("a config named like the clone directory while the origin says otherwise", func(t *testing.T) {
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "api.yaml"), "project_id: api\nproject_origin: other/api\n")
		allowed(t, home, plain(t, "api", "https://github.com/acme/widgets.git"))
	})

	t.Run("a machine with no config directory", func(t *testing.T) {
		allowed(t, t.TempDir(), plain(t, "widgets", "https://github.com/acme/widgets.git"))
	})

	t.Run("a config for another repo, however it names itself", func(t *testing.T) {
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "other.yaml"), "project_id: other\nproject_origin: acme/other\n")
		allowed(t, home, plain(t, "widgets", "https://github.com/acme/widgets.git"))
	})
}
