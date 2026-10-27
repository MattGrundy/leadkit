package team

// `leadkit team fetch-reference` downloads a community agent-def collection into
// <team_dir>/reference/<name>/ — vendored + cached in the repo so `team validate` is
// deterministic and offline afterwards.
//
// SECURITY: a fetched collection is UNTRUSTED DATA. This file only ever (a) shallow
// git-clones a repo URL into a SCRATCH temp dir, (b) copies out files whose name ends
// in ".md" — nothing else is read, copied, or (least of all) executed — and (c)
// deletes the scratch clone when done. Markdown content is later PARSED as YAML
// frontmatter + plain text by validate.go; it is never interpreted as code, never
// shelled out to, never templated into a command.

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/redact"
)

// DefaultReferenceRepo/DefaultReferenceRef are fetch-reference's defaults — VoltAgent's
// awesome-claude-code-subagents, tracking its default branch. Pass Ref as an exact
// tag/sha for a reproducible pin; the resolved commit is always recorded in
// SOURCE.txt regardless, so even a "main" fetch documents exactly what was pulled.
const (
	DefaultReferenceRepo = "https://github.com/VoltAgent/awesome-claude-code-subagents"
	DefaultReferenceRef  = "main"
)

// FetchReferenceOptions controls `team fetch-reference`.
type FetchReferenceOptions struct {
	RepoURL string // default DefaultReferenceRepo
	Ref     string // default DefaultReferenceRef — a branch/tag to shallow-clone
	Subdir  string // "" = whole repo; else only files under this path are considered
	Name    string // reference collection dir name under <team_dir>/reference/; derived from RepoURL if empty
	TeamDir string
}

// FetchResult is what fetch-reference actually pulled.
type FetchResult struct {
	Name        string
	Dir         string   // <team_dir>/reference/<name>/
	Files       []string // relative .md paths copied in, sorted
	ResolvedRef string   // the actual commit sha checked out
	FetchedAt   string   // RFC3339
}

// gitFetchFunc shallow-clones repoURL at ref into a fresh scratch dir it creates and
// returns (caller is responsible for removing it), plus the resolved commit sha.
// Injectable so tests never hit the real network.
type gitFetchFunc func(repoURL, ref string) (clonedDir, resolvedRef string, err error)

var gitFetch gitFetchFunc = defaultGitFetch

func defaultGitFetch(repoURL, ref string) (string, string, error) {
	scratch, err := os.MkdirTemp("", "leadkit-team-fetch-*")
	if err != nil {
		return "", "", err
	}
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, repoURL, scratch)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		os.RemoveAll(scratch)
		return "", "", fmt.Errorf("git clone %s (ref %q): %w: %s", redact.Origin(repoURL), ref, err, redact.URLCredentials(strings.TrimSpace(string(out))))
	}
	resolved := ref
	if out, err := exec.Command("git", "-C", scratch, "rev-parse", "HEAD").Output(); err == nil {
		resolved = strings.TrimSpace(string(out))
	}
	return scratch, resolved, nil
}

// FetchReference clones opts.RepoURL at opts.Ref, vendors every *.md file under it
// (optionally scoped to opts.Subdir) into <TeamDir>/reference/<name>/, writes
// SOURCE.txt recording exactly what was fetched, and discards the scratch clone.
// Re-running replaces the prior snapshot for that name (idempotent).
func FetchReference(opts FetchReferenceOptions) (FetchResult, error) {
	repoURL := opts.RepoURL
	if repoURL == "" {
		repoURL = DefaultReferenceRepo
	}
	ref := opts.Ref
	if ref == "" {
		ref = DefaultReferenceRef
	}
	name := opts.Name
	if name == "" {
		name = DeriveRepoName(repoURL)
	}

	clonedDir, resolvedRef, err := gitFetch(repoURL, ref)
	if err != nil {
		return FetchResult{}, fmt.Errorf("team fetch-reference: %w", err)
	}
	defer os.RemoveAll(clonedDir)

	destDir := filepath.Join(opts.TeamDir, "reference", name)
	if err := os.RemoveAll(destDir); err != nil {
		return FetchResult{}, fmt.Errorf("team fetch-reference: clear %s: %w", destDir, err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return FetchResult{}, fmt.Errorf("team fetch-reference: mkdir %s: %w", destDir, err)
	}

	searchRoot := clonedDir
	if opts.Subdir != "" {
		searchRoot = filepath.Join(clonedDir, opts.Subdir)
	}

	var files []string
	walkErr := filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort — skip whatever the walk can't reach
		}
		if d.IsDir() {
			if path != searchRoot && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // .git, .github, .claude — never vendored
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil // SECURITY: only .md — everything else in the clone is ignored
		}
		rel, relErr := filepath.Rel(clonedDir, path)
		if relErr != nil {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		destPath := filepath.Join(destDir, rel)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, data, 0o644); err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if walkErr != nil {
		return FetchResult{}, fmt.Errorf("team fetch-reference: walk clone: %w", walkErr)
	}
	sort.Strings(files)

	fetchedAt := nowFunc().UTC().Format(time.RFC3339)
	source := fmt.Sprintf("repo: %s\nref: %s\nresolved: %s\nfetched_at: %s\nfiles: %d\n",
		repoURL, ref, resolvedRef, fetchedAt, len(files))
	if err := os.WriteFile(filepath.Join(destDir, "SOURCE.txt"), []byte(source), 0o644); err != nil {
		return FetchResult{}, fmt.Errorf("team fetch-reference: write SOURCE.txt: %w", err)
	}

	return FetchResult{Name: name, Dir: destDir, Files: files, ResolvedRef: resolvedRef, FetchedAt: fetchedAt}, nil
}

// DeriveRepoName turns a repo URL into its bare repo name, e.g.
// https://github.com/VoltAgent/awesome-claude-code-subagents -> awesome-claude-code-subagents.
func DeriveRepoName(repoURL string) string {
	u := strings.TrimSuffix(strings.TrimRight(repoURL, "/"), ".git")
	parts := strings.Split(u, "/")
	if len(parts) == 0 {
		return "reference"
	}
	name := parts[len(parts)-1]
	if name == "" {
		return "reference"
	}
	return name
}
