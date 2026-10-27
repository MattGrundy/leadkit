package queue

import (
	"os"
	"path/filepath"
)

// RelativeLinkTarget is the symlink target to write, from a link that will live in linkDir, so
// that it resolves to target.
//
// The kernel resolves a relative target against the directory that REALLY holds the link, so the
// two paths must be compared by their symlink-resolved spellings. filepath.Rel between two
// spellings of one place (macOS /var and /private/var, both reachable from $TMPDIR) yields a
// target with the wrong depth: it reads as correct and does not resolve, which strands the
// ticket behind a dangling link that every later verb reports as "ticket not found".
//
// target need not exist yet (its directory must). When either side cannot be resolved it falls
// back to an absolute target, which is spelling-proof.
func RelativeLinkTarget(linkDir, target string) string {
	realDir, err := filepath.EvalSymlinks(linkDir)
	if err != nil {
		return absOr(target)
	}
	realTarget, err := resolveAllowingMissingLeaf(target)
	if err != nil {
		return absOr(target)
	}
	rel, err := filepath.Rel(realDir, realTarget)
	if err != nil {
		return realTarget
	}
	return rel
}

// resolveAllowingMissingLeaf resolves p's symlinks, tolerating a leaf that does not exist yet.
func resolveAllowingMissingLeaf(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(abs)), nil
}

func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// LinkResolves reports whether a symlink with this target, placed in linkDir, would resolve to an
// existing file. It writes nothing, so a caller can refuse BEFORE it has mutated anything.
func LinkResolves(linkDir, linkTarget string) bool {
	if filepath.IsAbs(linkTarget) {
		_, err := os.Stat(linkTarget)
		return err == nil
	}
	realDir, err := filepath.EvalSymlinks(linkDir)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(realDir, linkTarget))
	return err == nil
}
