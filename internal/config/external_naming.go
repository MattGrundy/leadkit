package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"mgpp.dev/leadkit/internal/install"
)

// gitRemoteURLsDir lists the URL of EVERY remote of the repo containing dir, not only origin.
// An injectable var so tests are deterministic without a real git binary.
var gitRemoteURLsDir = func(dir string) []string {
	out, err := exec.Command("git", "-C", dir, "config", "--get-regexp", `^remote\..*\.url$`).Output()
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && strings.TrimSpace(v) != "" {
			urls = append(urls, strings.TrimSpace(v))
		}
	}
	return urls
}

// ExternalConfigsNamingRepo answers "does a config in configDir belong to the repo at repoRoot",
// by every handle the repo has, and not only the one the lookup used. The lookup finds
// <configDir>/<origin-name>.yaml, so a config is missed when the repo's origin was renamed or
// removed, or the clone's directory is not called what the config is: nothing is then found, and
// "found nothing" cannot be told from "there is nothing" by the lookup alone. It returns one line
// per file that names the repo, and per file it could not read, because a file that cannot be read
// may be the one:
//
//   - its name is the slug of the repo directory's name, or of any remote's repository name. These
//     are looked up by name, exactly as the lookup does (a stat of <configDir>/<name>.yaml), and a
//     file found that way names the repo without being read: it need not parse, be readable, or be
//     spelled in the case the slug is (a case-insensitive filesystem finds it, as the lookup would),
//     and the directory need not be listable;
//   - its project_origin matches any remote's URL (OriginMatchesProject);
//   - its project_id is one of those names. These two need the file's contents, so they need the
//     directory listed and the file read; a config that has only these handles and sits in an
//     unlistable directory is invisible here, as it was to the lookup.
//
// It cannot see a config that shares none of these handles with the repo (every remote removed,
// a differently-named clone and no project_origin), and a client-side check cannot be made to.
func ExternalConfigsNamingRepo(configDir, repoRoot string) []string {
	return externalConfigsNaming(configDir, repoRoot, true)
}

// ExternalConfigsNamingRepoByRemote is ExternalConfigsNamingRepo for a repo with no other sign of
// being a leadkit project, where the check runs on every push on the machine and so may only
// refuse on a definite match. It names the repo only by what a remote says (the repository name
// of any remote's URL, or a project_origin matching one), never by the clone directory's name,
// which says nothing about which project a clone is, and it says nothing about a file it cannot
// read or parse or a directory it cannot list when that file is not named for a remote: that is
// not evidence about this repo.
func ExternalConfigsNamingRepoByRemote(configDir, repoRoot string) []string {
	return externalConfigsNaming(configDir, repoRoot, false)
}

// externalConfigsNaming is the shared reading. broad adds the clone directory's name as a handle
// and reports what could not be read.
func externalConfigsNaming(configDir, repoRoot string, broad bool) []string {
	remotes := gitRemoteURLsDir(repoRoot)
	var order []string // the names, in the order they are reported
	names := map[string]bool{}
	add := func(n string) {
		if n != "" && !names[n] {
			names[n] = true
			order = append(order, n)
		}
	}
	if broad {
		add(install.Slugify(filepath.Base(repoRoot)))
	}
	for _, u := range remotes {
		add(install.OriginRepoName(u))
	}

	// The dominant handle: a file named for the repo. Looked up by name, as the lookup does, so
	// neither the listing nor the file's contents can hide it.
	var found []string
	var named []os.FileInfo
	for _, n := range order {
		path := filepath.Join(configDir, n+".yaml")
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			found = append(found, fmt.Sprintf("%s is named for this repo (%q)", path, n))
			named = append(named, fi)
		}
	}

	// The other handles are inside files, so they need the directory listed.
	entries, err := os.ReadDir(configDir)
	if err != nil {
		if broad {
			found = append(found, fmt.Sprintf("the config directory %s could not be listed: %v", configDir, err))
		}
		return found
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(configDir, e.Name())
		if info, err := e.Info(); err == nil && isAmong(info, named) {
			continue // already reported by name
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if broad {
				found = append(found, fmt.Sprintf("%s could not be read: %v", path, err))
			}
			continue
		}
		var ids struct {
			ProjectOrigin string `yaml:"project_origin"`
			ProjectID     string `yaml:"project_id"`
		}
		if err := yaml.Unmarshal(data, &ids); err != nil {
			if broad {
				found = append(found, fmt.Sprintf("%s could not be parsed, so it cannot be ruled out: %v", path, err))
			}
			continue
		}
		matched := false
		if ids.ProjectOrigin != "" {
			for _, u := range remotes {
				if OriginMatchesProject(u, ids.ProjectOrigin) {
					found = append(found, fmt.Sprintf("%s declares project_origin %q, which is a remote of this repo", path, RedactOrigin(ids.ProjectOrigin)))
					matched = true
					break
				}
			}
		}
		if !matched && ids.ProjectID != "" && names[install.Slugify(ids.ProjectID)] {
			found = append(found, fmt.Sprintf("%s declares project_id %q, which is this repo's name", path, ids.ProjectID))
		}
	}
	return found
}

// isAmong reports whether info is the same file as one of named (the same inode, so a file found
// by name is the same file whatever case the listing spells it in).
func isAmong(info os.FileInfo, named []os.FileInfo) bool {
	for _, n := range named {
		if os.SameFile(info, n) {
			return true
		}
	}
	return false
}
