package config

import (
	"os"
	"path/filepath"
)

// Locations is every config file leadkit could read for a directory, plus the one it
// ACTUALLY resolves. It exists so a lead can tell which file to edit (`leadkit config
// path`) and so doctor/whoami/config-show can name the would-be file under defaults.
// It is built from the SAME primitives Load() uses (discover, ExternalConfigTargetForDir,
// $LEADKIT_PROFILE), so it cannot drift from the real resolver.
type Locations struct {
	// EnvProfile is $LEADKIT_PROFILE ("" when unset) — wins over everything.
	EnvProfile string
	// WalkUp is the in-repo leadkit.yaml found by walking up from the dir ("" if none).
	WalkUp string
	// InRepoPath is <repo-root>/leadkit.yaml, the in-repo location to create.
	InRepoPath   string
	InRepoExists bool
	// ExternalPath is ~/.claude/leadkit/config/<origin-slug>.yaml, whether or not it exists.
	ExternalPath   string
	ExternalExists bool
	// Slug is the origin-based key used for ExternalPath; RepoRoot the repo it governs.
	Slug     string
	RepoRoot string
	// Active is the file Load() resolves ("" = built-in defaults); ActiveKind is one of
	// "env", "in-repo", "external", "defaults".
	Active     string
	ActiveKind string
}

// ResolveLocations computes the Locations for dir, mirroring load()'s precedence:
// $LEADKIT_PROFILE > in-repo walk-up > external <origin-slug>.yaml > defaults.
func ResolveLocations(dir string) Locations {
	var l Locations
	l.EnvProfile = os.Getenv("LEADKIT_PROFILE")
	l.ExternalPath, l.RepoRoot, l.Slug = ExternalConfigTargetForDir(dir)
	if l.ExternalPath != "" {
		if fi, err := os.Stat(l.ExternalPath); err == nil && !fi.IsDir() {
			l.ExternalExists = true
		}
	}
	root := l.RepoRoot
	if root == "" {
		root = dir
	}
	if root != "" {
		l.InRepoPath = filepath.Join(root, DefaultProfileName)
		if fi, err := os.Stat(l.InRepoPath); err == nil && !fi.IsDir() {
			l.InRepoExists = true
		}
	}
	if dir != "" {
		l.WalkUp, _ = discover(dir)
	}
	switch {
	case l.EnvProfile != "":
		l.Active, l.ActiveKind = l.EnvProfile, "env"
	case l.WalkUp != "":
		l.Active, l.ActiveKind = l.WalkUp, "in-repo"
	case l.ExternalExists:
		l.Active, l.ActiveKind = l.ExternalPath, "external"
	default:
		l.ActiveKind = "defaults"
	}
	return l
}

// DefaultsHint is the one-line note shown wherever the profile resolved to built-in
// defaults: the external file leadkit would read if created, and the slug caveat.
func (l Locations) DefaultsHint() string {
	if l.ExternalPath == "" {
		return ""
	}
	return "would read " + l.ExternalPath + " if created (`leadkit config init --external`); " +
		"until then the ambient/policy slug falls back to the dir basename — see `leadkit config path`"
}

// FormatPath renders `leadkit config path`: every candidate file with [exists]/[not
// created], the $LEADKIT_PROFILE override when set, and the ACTIVE line.
func FormatPath(l Locations) string {
	mark := func(ok bool) string {
		if ok {
			return "[exists]"
		}
		return "[not created]"
	}
	var b []byte
	add := func(s string) { b = append(b, s...); b = append(b, '\n') }
	if l.EnvProfile != "" {
		add("$LEADKIT_PROFILE: " + l.EnvProfile + " " + mark(fileExists(l.EnvProfile)) + "  (override: wins over both files below)")
	}
	if l.ExternalPath != "" {
		add("external:         " + l.ExternalPath + " " + mark(l.ExternalExists) + "  (keyed by origin repo-name \"" + l.Slug + "\")")
	} else {
		add("external:         (no repo/slug resolvable from this directory)")
	}
	if l.InRepoPath != "" {
		add("in-repo:          " + l.InRepoPath + " " + mark(l.InRepoExists) + "  (wins over external if both exist)")
	}
	if l.WalkUp != "" && l.WalkUp != l.InRepoPath {
		add("in-repo (walk-up): " + l.WalkUp + " [exists]  (an ancestor's leadkit.yaml — found by walking up)")
	}
	if l.Active != "" {
		add("ACTIVE: " + l.Active + "  (" + l.ActiveKind + ")")
	} else {
		add("ACTIVE: defaults (no config file)")
		if l.ExternalPath != "" {
			add("  create the external one with: leadkit config init --external")
		}
	}
	return string(b)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
