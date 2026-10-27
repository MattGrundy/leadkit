package doctor

// binary-skew (docs/build-tasks/burn-auto-enforce.md): the exact trap. A session
// ran a STALE second `leadkit` (an old `go install` build at ~/go/bin/leadkit that
// returns a burn verdict but does NOT write the workstream registry and lacks every
// feature since) while the current binary sat at ~/.local/bin/leadkit. Two `leadkit` on
// PATH with divergent behaviour is a silent version-skew trap — not just for burn — so
// doctor surfaces it. Read-only, and NEVER a hard error (a WARN at most).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BinarySkewInput is binarySkewCheck's fully-injectable input — real PATH + fs in
// production, a fake PATH string + fake stat/eval in tests. No global state is read.
type BinarySkewInput struct {
	// PathEnv is $PATH (os.PathListSeparator-joined dirs), walked IN ORDER — the first
	// existing `leadkit` in it is the one the shell resolves.
	PathEnv string
	// ExtraCandidates are absolute paths also probed even if not on PATH — the common
	// stale-binary locations (~/go/bin/leadkit, ~/.local/bin/leadkit,
	// ~/.claude/leadkit-bin) — so a binary lurking off-PATH is still surfaced.
	ExtraCandidates []string
	// Self is the currently-running (deployed) binary's real path, "" if unknown — used
	// only to LABEL which discovered binary is the deployed one, never to gate the WARN.
	Self string
	// EvalSymlinks resolves a path to its real target (filepath.EvalSymlinks in prod);
	// on error the candidate's own path is used as its identity.
	EvalSymlinks func(string) (string, error)
	// Stat reports whether a candidate exists (os.Stat in prod).
	Stat func(string) (os.FileInfo, error)
}

// RealBinarySkewInput builds the production BinarySkewInput from the live environment.
func RealBinarySkewInput() BinarySkewInput {
	home := os.Getenv("HOME")
	self := ""
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			self = real
		} else {
			self = exe
		}
	}
	return BinarySkewInput{
		PathEnv: os.Getenv("PATH"),
		ExtraCandidates: []string{
			filepath.Join(home, "go", "bin", "leadkit"),
			filepath.Join(home, ".local", "bin", "leadkit"),
			filepath.Join(home, ".claude", "leadkit-bin", "leadkit"),
		},
		Self:         self,
		EvalSymlinks: filepath.EvalSymlinks,
		Stat:         os.Stat,
	}
}

// foundBinary is one discovered `leadkit` executable and its resolved real path.
type foundBinary struct {
	path     string // as discovered (a PATH dir + "leadkit", or an extra candidate)
	real     string // EvalSymlinks(path), or path itself on resolve failure
	onPath   bool   // discovered by walking PathEnv (vs an off-PATH extra)
	deployed bool   // real == resolved Self
}

// realPath resolves p to its real target, falling back to p itself on failure (or when no
// resolver is injected) — a candidate's own path is then its identity.
func (in BinarySkewInput) realPath(p string) string {
	if in.EvalSymlinks != nil {
		if r, err := in.EvalSymlinks(p); err == nil {
			return r
		}
	}
	return p
}

// installCandidates is the ONE enumeration of "where a leadkit binary may live": every
// existing `leadkit` on PathEnv, walked in order, then the off-PATH ExtraCandidates, each
// with its resolved real path. binarySkewCheck reports on it and the hook probe uses it as
// the bound on what doctor may execute, so the two cannot drift on what counts as an
// installed leadkit.
func installCandidates(in BinarySkewInput) []foundBinary {
	selfReal := ""
	if in.Self != "" {
		selfReal = in.realPath(in.Self)
	}

	exists := func(p string) bool {
		if in.Stat == nil {
			return false
		}
		fi, err := in.Stat(p)
		return err == nil && !fi.IsDir()
	}

	var found []foundBinary
	seenPath := map[string]bool{} // dedupe the same discovered path appearing twice

	add := func(p string, onPath bool) {
		if p == "" || seenPath[p] || !exists(p) {
			return
		}
		seenPath[p] = true
		real := in.realPath(p)
		found = append(found, foundBinary{path: p, real: real, onPath: onPath, deployed: selfReal != "" && real == selfReal})
	}

	// PATH first, in order — the FIRST existing entry is what the shell runs.
	for _, dir := range strings.Split(in.PathEnv, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		add(filepath.Join(dir, "leadkit"), true)
	}
	// Then the off-PATH extras (a stale binary that isn't even on PATH is still a trap).
	for _, c := range in.ExtraCandidates {
		add(c, false)
	}
	return found
}

// binarySkewCheck (pure) walks the PATH + extra candidates (installCandidates), and
// reports:
//   - OK/Info when zero or one distinct real file backs every discovered binary.
//   - WARN when two or more DISTINCT real files compete — naming both paths, which one
//     wins (first on PATH), which is the deployed one, and the remedy. Never critical.
func binarySkewCheck(in BinarySkewInput) Check {
	found := installCandidates(in)

	if len(found) == 0 {
		return Check{Name: "binary-skew", OK: true, Severity: SeverityInfo, Detail: "no leadkit binary found on PATH or in the common install locations"}
	}

	distinct := map[string]bool{}
	for _, f := range found {
		distinct[f.real] = true
	}
	if len(distinct) == 1 {
		// One binary, or several paths that all resolve to the same real file (e.g. a
		// symlink) — no skew.
		if len(found) == 1 {
			return Check{Name: "binary-skew", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("one leadkit on PATH: %s -> %s", found[0].path, found[0].real)}
		}
		paths := make([]string, 0, len(found))
		for _, f := range found {
			paths = append(paths, f.path)
		}
		return Check{Name: "binary-skew", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("%d leadkit paths all resolve to the same real file %s: %s", len(found), found[0].real, strings.Join(paths, ", "))}
	}

	// Two or more DISTINCT real files — a genuine skew trap. Name every discovered
	// binary, mark the first-on-PATH winner + the deployed one, and give the remedy.
	var winner *foundBinary
	for i := range found {
		if found[i].onPath {
			winner = &found[i]
			break
		}
	}
	var parts []string
	for i := range found {
		f := found[i]
		labels := []string{}
		if winner != nil && f.path == winner.path {
			labels = append(labels, "first on PATH — WINS")
		} else if !f.onPath {
			labels = append(labels, "off-PATH")
		}
		if f.deployed {
			labels = append(labels, "deployed/running")
		}
		suffix := ""
		if len(labels) > 0 {
			suffix = " (" + strings.Join(labels, ", ") + ")"
		}
		parts = append(parts, fmt.Sprintf("%s -> %s%s", f.path, f.real, suffix))
	}
	detail := "multiple leadkit binaries with DIVERGENT real paths: " + strings.Join(parts, "; ") +
		". The stale binary lacks workstream registration + recent features and silently escapes governance. " +
		"Remedy: remove the stale ~/go/bin/leadkit, or fix PATH order so ~/.local/bin precedes ~/go/bin."
	return Check{Name: "binary-skew", OK: false, Severity: SeverityWarn, Detail: detail}
}
