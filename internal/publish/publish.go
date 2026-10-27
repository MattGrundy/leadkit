// Package publish prepares and verifies the public tree of a repository that is developed in
// private. It is the committed form of a check that used to live in session scratch and was lost
// twice in one day, taking its term list with it. A gate whose configuration evaporates is not a
// gate, so the logic is here and the term list resolves from a durable source (see
// scrubterms.Resolve).
//
// What it does, in order:
//
//  1. Resolve the scrub term list, and REFUSE when it does not resolve or when the parser kept
//     fewer terms than the file has entries. Neither is reported as clean.
//  2. `git archive` the ref into a clean directory.
//  3. Remove the paths in the committed exclusion manifest, and report any listed path that no
//     longer exists as STALE.
//  4. Search every remaining file, name and symlink for the terms, with no extension filter.
//  5. Search the source history's messages and identities. That half is informational by default:
//     a private history is expected to carry matches, and the answer to it is the next step, not
//     a failed run.
//  6. (prepare) Build the public repository as ONE new commit, then run both halves of the check
//     again inside it before anything is reported as ready.
//
// What it never does: create a repository, add a remote, push, or touch any forge. Every command
// goes through Guard, which allows only reading the source and building a local repository, and
// takes no option that lifts it. Publishing is owner-direct (tickets 198 and 267); the tool
// prepares and verifies, and a human does the rest.
//
// # The second release
//
// The public repository's history is deliberately not the private one, so a release is either
// "replace the tree with a new single commit" or "commit the delta under a generic message". It is
// the DELTA, on top of the existing public history, with a message the tool generates:
//
//   - Replacing the history rewrites commits other people have cloned. Their clones diverge
//     from the published one and every fork keeps the root it cloned; a published root
//     cannot be recalled.
//   - The delta cannot leak a private commit message, because no private message is ever read
//     into it: the message is generated, and both halves of the check run over the result.
//
// What a delta costs is granularity: the public log reads one commit per release, not one per
// change. That is the price of never carrying private history.
//
// The release commit makes the public tree EQUAL the source tree (it is a full overwrite, not a
// merge), so a change made only on the public side is reverted; the report counts what is added,
// changed and removed and warns on every file that is in the public repository and not in the
// source. Because a delta can never remove an old file from an earlier commit, `--base` also
// searches the FILES of every past tree and refuses on a match: the only remedy for a term
// already in public history is to rewrite it, which is the owner's decision.
package publish

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// DefaultManifest is where the exclusion manifest lives in the tree being published. It is
// read from the archived ref, so the manifest that applies is the one committed with the code.
const DefaultManifest = "docs/publish-exclude.txt"

const (
	defaultAuthorName  = "leadkit publish"
	defaultAuthorEmail = "publish@localhost.invalid"
)

// Options select what to verify or build.
type Options struct {
	Repo     string // the source checkout; default "."
	Ref      string // the commit to publish; default HEAD
	Manifest string // path of the exclusion manifest within the tree; default DefaultManifest

	// Out is a new directory to build the public repository in (the first release). Base is an
	// fresh clone of the public repository; one new commit makes its tree equal the source tree. Prepare
	// takes exactly one of them; Check takes neither.
	Out  string
	Base string

	Message     string // the release commit message; default is generated
	AuthorName  string
	AuthorEmail string

	// RequireCleanHistory makes a match in the source history a failure. It is off by default
	// because a private history is expected to carry matches.
	RequireCleanHistory bool
}

// Deps are the seams. Git and GH are wrapped in Guard whatever the caller passes.
type Deps struct {
	Git Runner
	GH  Runner
	// Terms says where to look for the term list; its Runner is replaced by the guarded GH.
	Terms   scrubterms.ResolveOptions
	Now     func() time.Time
	TempDir string // where scratch trees go; default os.TempDir()
}

// Report is the outcome of a run. A refusal (the term list does not resolve, bad arguments, git
// failing) is returned as an error instead, and there is no Report to read as clean.
type Report struct {
	Ref, SHA    string
	TermsSource string
	TermCount   int

	Manifest      string
	Excluded      []string // manifest entries that were present and removed
	ExcludedFiles int
	Stale         []string // manifest entries that match nothing in the tree

	Files        int // files and links searched, after exclusion
	TreeFindings []Finding
	// NotText are the searched files that are not UTF-8 text, so a term inside their encoded
	// content would not be seen. Reported on every run; it does not fail one.
	NotText []string

	SourceHistory       HistoryResult
	RequireCleanHistory bool

	Built *Built // nil for Check, and for a Prepare that stopped before building
}

// Built is the repository Prepare made, and what the re-check inside it found.
type Built struct {
	Dir     string
	Mode    string // "first release" or "update"
	Commit  string
	Commits int // commits in the repository afterwards

	// The release commit makes the public tree EQUAL the source tree (update only): files in the
	// source and not the public checkout are Added, differing ones Changed, and those in the
	// public checkout and NOT in the source are Removed, which would discard a public-only change.
	Added, Changed int
	Removed        []string
	TreesSearched  int // distinct trees of the repository whose files were searched

	Files           int
	TreeFindings    []Finding
	HistoryFindings []Finding
	Problems        []string // anything else that did not hold
	Undone          string   // what was removed or rolled back after a failure
}

// OK reports whether the run found nothing that blocks publishing.
func (r Report) OK() bool {
	if len(r.TreeFindings) > 0 || len(r.Stale) > 0 {
		return false
	}
	if r.RequireCleanHistory && len(r.SourceHistory.Findings) > 0 {
		return false
	}
	if r.Built != nil && len(r.Built.TreeFindings)+len(r.Built.HistoryFindings)+len(r.Built.Problems) > 0 {
		return false
	}
	return true
}

type assessment struct {
	rep     Report
	terms   scrubterms.Resolved
	staging string
	files   []string
	cleanup func()
	git     Runner
	opts    Options
	deps    Deps
	manif   []string
	// seenTrees are the trees of the repository being built on whose files have been searched.
	seenTrees map[string]bool
}

// Check verifies the tree and searches the history, and writes nothing outside a scratch
// directory it removes.
func Check(o Options, d Deps) (Report, error) {
	a, err := assess(o, d)
	if err != nil {
		return Report{}, err
	}
	defer a.cleanup()
	return a.rep, nil
}

// Prepare is Check, and when the tree is clean builds the public repository and re-checks it.
func Prepare(o Options, d Deps) (Report, error) {
	if (o.Out == "") == (o.Base == "") {
		return Report{}, errors.New("prepare needs exactly one of --out (a new directory, the first release) " +
			"or --base (a clean checkout of the existing public repository, an update)")
	}
	a, err := assess(o, d)
	if err != nil {
		return Report{}, err
	}
	defer a.cleanup()
	if len(a.rep.TreeFindings) > 0 || len(a.rep.Stale) > 0 ||
		(o.RequireCleanHistory && len(a.rep.SourceHistory.Findings) > 0) {
		return a.rep, nil
	}
	if o.Out != "" {
		a.rep.Built, err = a.buildFirst()
	} else {
		a.rep.Built, err = a.buildUpdate()
	}
	return a.rep, err
}

func assess(o Options, d Deps) (*assessment, error) {
	git := Guard(d.Git)
	if d.Git == nil {
		return nil, errors.New("publish: no git runner")
	}
	if o.Repo == "" {
		o.Repo = "."
	}
	if o.Ref == "" {
		o.Ref = "HEAD"
	}
	if strings.HasPrefix(o.Ref, "-") {
		return nil, fmt.Errorf("ref %q looks like an option", o.Ref)
	}
	if o.Manifest == "" {
		o.Manifest = DefaultManifest
	}
	if o.AuthorName == "" {
		o.AuthorName = defaultAuthorName
	}
	if o.AuthorEmail == "" {
		o.AuthorEmail = defaultAuthorEmail
	}
	if d.Now == nil {
		d.Now = time.Now
	}

	// The term list first. Nothing below runs without one, because a search with no terms finds
	// nothing and would be reported as clean.
	terms, err := scrubterms.Resolve(termOptions(d))
	if err != nil {
		return nil, err
	}

	sha, err := revParse(git, o.Repo, o.Ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s in %s: %w", o.Ref, o.Repo, err)
	}
	tarball, stderr, err := git.Run([]string{"git", "-C", o.Repo, "archive", "--format=tar", sha}, nil)
	if err != nil {
		return nil, fmt.Errorf("git archive %s: %w: %s", sha, err, strings.TrimSpace(string(stderr)))
	}
	staging, err := os.MkdirTemp(d.TempDir, "leadkit-publish-")
	if err != nil {
		return nil, err
	}
	a := &assessment{
		terms: terms, staging: staging, git: git, opts: o, deps: d,
		cleanup: func() { _ = os.RemoveAll(staging) },
	}
	fail := func(err error) (*assessment, error) { a.cleanup(); return nil, err }

	if err := extractTar(tarball, staging); err != nil {
		return fail(err)
	}
	before, err := listTree(staging)
	if err != nil {
		return fail(err)
	}
	mdata, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(o.Manifest)))
	if err != nil {
		return fail(fmt.Errorf("exclusion manifest %s is not in %s: %w "+
			"(the exclusion list lives in a committed manifest, not in a script)", o.Manifest, short(sha), err))
	}
	a.manif, err = ParseManifest(string(mdata))
	if err != nil {
		return fail(fmt.Errorf("exclusion manifest %s: %w", o.Manifest, err))
	}
	stale, err := applyManifest(staging, a.manif)
	if err != nil {
		return fail(err)
	}
	after, err := listTree(staging)
	if err != nil {
		return fail(err)
	}
	a.files = after

	findings, nfiles, opaque, err := ScanTree(staging, terms.Terms)
	if err != nil {
		return fail(err)
	}
	hist, err := ScanHistory(git, o.Repo, []string{sha}, terms.Terms)
	if err != nil {
		return fail(err)
	}

	staleSet := map[string]bool{}
	for _, s := range stale {
		staleSet[s] = true
	}
	var present []string
	for _, p := range a.manif {
		if !staleSet[p] {
			present = append(present, p)
		}
	}
	a.rep = Report{
		Ref: o.Ref, SHA: sha, TermsSource: terms.Source, TermCount: terms.Count(),
		Manifest: o.Manifest, Excluded: present, ExcludedFiles: len(before) - len(after), Stale: stale,
		Files: nfiles, TreeFindings: findings, NotText: opaque,
		SourceHistory: hist, RequireCleanHistory: o.RequireCleanHistory,
	}
	return a, nil
}

// termOptions is where the term list is looked for. The GitHub runner is wrapped in Guard here,
// so reading the variable is the only thing resolution can ask gh to do.
func termOptions(d Deps) scrubterms.ResolveOptions {
	o := d.Terms
	o.Runner = nil
	if d.GH != nil {
		o.Runner = Guard(d.GH)
	}
	return o
}

func revParse(git Runner, repo string, args ...string) (string, error) {
	out, stderr, err := git.Run(append([]string{"git", "-C", repo, "rev-parse", "--verify", "-q"}, args...), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (a *assessment) gitOut(dir string, args ...string) (string, error) {
	out, stderr, err := a.git.Run(append([]string{"git", "-C", dir}, args...), nil)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(stderr)))
	}
	return string(out), nil
}

var errNothingToRelease = errors.New("the tree is identical to the repository's current tree; there is nothing to release")

func (a *assessment) commit(dir, message string) error {
	if _, err := a.gitOut(dir, "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.excludesFile=/dev/null", "add", "-A"); err != nil {
		return err
	}
	if st, err := a.gitOut(dir, "status", "--porcelain"); err != nil {
		return err
	} else if strings.TrimSpace(st) == "" {
		return errNothingToRelease
	}
	ident := a.opts.AuthorName + " <" + a.opts.AuthorEmail + ">"
	_, err := a.gitOut(dir,
		"-c", "user.name="+a.opts.AuthorName, "-c", "user.email="+a.opts.AuthorEmail,
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		"commit", "-q", "--no-verify", "--no-gpg-sign", "--author", ident, "-m", message)
	return err
}

// toplevel is the real path of the repository root containing dir.
func (a *assessment) toplevel(dir string) (string, error) {
	out, err := a.gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSpace(out))
}

func (a *assessment) buildFirst() (*Built, error) {
	o := a.opts
	out, err := filepath.Abs(o.Out)
	if err != nil {
		return nil, err
	}
	if entries, rerr := os.ReadDir(out); rerr == nil && len(entries) > 0 {
		return nil, fmt.Errorf("--out %s exists and is not empty; the first release builds in a new directory", out)
	}
	if src, terr := a.toplevel(o.Repo); terr == nil {
		if inside(src, out) {
			return nil, fmt.Errorf("--out %s is inside the source repository %s", out, src)
		}
	}
	_, statErr := os.Stat(out)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	b := &Built{Dir: out, Mode: "first release"}
	undo := func() {
		if statErr == nil {
			// The directory was the caller's (empty); leave it, remove what was put in it.
			entries, _ := os.ReadDir(out)
			for _, e := range entries {
				_ = os.RemoveAll(filepath.Join(out, e.Name()))
			}
			b.Undone = "emptied " + out
			return
		}
		_ = os.RemoveAll(out)
		b.Undone = "removed " + out
	}
	if err := copyTree(a.staging, out); err != nil {
		undo()
		return nil, err
	}
	if _, err := a.gitOut(out, "init", "-q", "-b", "main", "--template="); err != nil {
		undo()
		return nil, err
	}
	msg := o.Message
	if msg == "" {
		msg = "Initial public release"
	}
	if err := a.commit(out, msg); err != nil {
		undo()
		return nil, err
	}
	a.verify(b, 1)
	if len(b.TreeFindings)+len(b.HistoryFindings)+len(b.Problems) > 0 {
		undo()
	}
	return b, nil
}

func (a *assessment) buildUpdate() (*Built, error) {
	o := a.opts
	base, err := filepath.Abs(o.Base)
	if err != nil {
		return nil, err
	}
	if fi, serr := os.Stat(filepath.Join(base, ".git")); serr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("--base %s is not the top of a git checkout", base)
	}
	realBase, err := a.toplevel(base)
	if err != nil {
		return nil, err
	}
	if realSrc, serr := a.toplevel(o.Repo); serr == nil && realSrc == realBase {
		return nil, fmt.Errorf("--base %s is the source repository; it must be a checkout of the PUBLIC repository", base)
	}
	prev, err := revParse(a.git, base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("--base %s has no commits; use --out for the first release: %w", base, err)
	}
	// Nothing may be modified, untracked OR ignored. Ignored files are invisible to a plain status
	// and are exactly what a release would overwrite or a rollback would delete (the checkout's
	// own .gitignore is replaced by the release's, so a file it hid becomes visible to the commit).
	// A fresh clone has none; if this refuses, use one.
	if st, serr := a.gitOut(base, "status", "--porcelain", "--ignored"); serr != nil {
		return nil, serr
	} else if strings.TrimSpace(st) != "" {
		return nil, fmt.Errorf("--base %s is not a fresh checkout: it has modified, untracked or ignored files "+
			"(e.g. %s). A release would overwrite them or a rollback would delete them; use a fresh clone",
			base, strings.TrimSpace(strings.SplitN(strings.TrimSpace(st), "\n", 2)[0]))
	}
	cnt, err := a.gitOut(base, "rev-list", "--count", "--all")
	if err != nil {
		return nil, err
	}
	prevCount, cerr := strconv.Atoi(strings.TrimSpace(cnt))
	if cerr != nil {
		return nil, fmt.Errorf("cannot read the commit count of %s: %q", base, cnt)
	}

	// Refuse before writing anything if the public history is already unclean: a release must
	// not be stacked on it.
	hist, err := ScanHistory(a.git, base, []string{"--all"}, a.terms.Terms)
	if err != nil {
		return nil, err
	}
	// And the FILES of every past release: a delta never removes an old blob, so a term that
	// shipped before it was on the list is findable in that commit for good.
	a.seenTrees = map[string]bool{}
	past, err := ScanHistoryTrees(a.git, base, []string{"--all"}, a.terms.Terms, a.seenTrees)
	if err != nil {
		return nil, err
	}
	refs, unreadable, err := ScanRefs(a.git, base, a.terms.Terms)
	if err != nil {
		return nil, err
	}
	b := &Built{Dir: base, Mode: "update", TreesSearched: len(a.seenTrees)}
	for _, u := range unreadable {
		b.Problems = append(b.Problems, "the ref "+u+" holds content this check does not read, so the history cannot be called clean; nothing was written")
	}
	found := append(append(hist.Findings, past.Findings...), refs...)
	if len(found) > 0 {
		b.HistoryFindings = found
		b.Problems = append(b.Problems, "the public repository's own history already matches (a message, an identity, a tag, "+
			"a ref name or a file in an earlier release); nothing was written, and a release must not be stacked on it. "+
			"A delta cannot remove it: the only way out is to rewrite the published history, which is the owner's "+
			"decision and not this tool's")
	}
	if len(found) > 0 || len(unreadable) > 0 {
		return b, nil
	}

	// Wipe what is tracked, copy the new tree over, commit. The checkout holds nothing else (checked
	// above), and copyTree refuses to write over anything that is still there.
	tracked, err := a.gitOut(base, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	if err := a.summarise(b, base, tracked); err != nil {
		return nil, err
	}
	rollback := func() {
		_, _ = a.gitOut(base, "reset", "-q", "--hard", prev)
		_, _ = a.gitOut(base, "clean", "-fdq")
		b.Undone = "rolled back to " + short(prev)
	}
	for _, f := range strings.Split(tracked, "\x00") {
		if f == "" {
			continue
		}
		if rerr := os.Remove(filepath.Join(base, filepath.FromSlash(f))); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			rollback()
			return nil, rerr
		}
	}
	if err := copyTree(a.staging, base); err != nil {
		rollback()
		return nil, err
	}
	msg := o.Message
	if msg == "" {
		msg = "Release " + a.deps.Now().UTC().Format("2006-01-02")
	}
	if err := a.commit(base, msg); err != nil {
		rollback()
		return nil, err
	}
	a.verify(b, prevCount+1)
	if len(b.TreeFindings)+len(b.HistoryFindings)+len(b.Problems) > 0 {
		rollback()
	}
	return b, nil
}

// summarise compares the tracked files of the public checkout with the tree about to replace
// them, BEFORE anything is written, and records what the release will add, change and remove.
// The release commit makes the public tree equal the source tree on purpose; the cost is that a
// change made only on the public side (a merged contribution, a LICENSE, CI configuration) is
// reverted, so the files that exist in the checkout and not in the source are named.
func (a *assessment) summarise(b *Built, base, tracked string) error {
	inBase := map[string]bool{}
	for _, f := range strings.Split(tracked, "\x00") {
		if f == "" {
			continue
		}
		inBase[f] = true
		old, err := fileSig(filepath.Join(base, filepath.FromSlash(f)))
		if err != nil {
			return err
		}
		cur, err := fileSig(filepath.Join(a.staging, filepath.FromSlash(f)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			b.Removed = append(b.Removed, f)
		case err != nil:
			return err
		case cur != old:
			b.Changed++
		}
	}
	for _, f := range a.files {
		if !inBase[f] {
			b.Added++
		}
	}
	sort.Strings(b.Removed)
	return nil
}

// fileSig is a file's identity for comparison: its content and executable bit, or its link target.
func fileSig(p string) (string, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t, err := os.Readlink(p)
		return "L:" + t, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("F:%v:%s", fi.Mode()&0o111 != 0, data), nil
}

func inside(parent, child string) bool {
	// child may not exist yet, so resolve its deepest existing ancestor.
	c := child
	var tail []string
	for {
		if r, err := filepath.EvalSymlinks(c); err == nil {
			c = r
			break
		}
		tail = append([]string{filepath.Base(c)}, tail...)
		next := filepath.Dir(c)
		if next == c {
			break
		}
		c = next
	}
	c = filepath.Join(append([]string{c}, tail...)...)
	rel, err := filepath.Rel(parent, c)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// verify runs both halves of the check inside the repository that was built, over what was
// COMMITTED rather than what is on disk, and records everything that did not hold.
func (a *assessment) verify(b *Built, wantCommits int) {
	problem := func(format string, args ...any) { b.Problems = append(b.Problems, fmt.Sprintf(format, args...)) }
	sha, err := revParse(a.git, b.Dir, "HEAD")
	if err != nil {
		problem("cannot read the new commit: %v", err)
		return
	}
	b.Commit = sha

	if n, err := a.gitOut(b.Dir, "rev-list", "--count", "--all"); err != nil {
		problem("%v", err)
	} else {
		b.Commits, _ = strconv.Atoi(strings.TrimSpace(n))
		if b.Commits != wantCommits {
			problem("the repository holds %d commits, expected %d", b.Commits, wantCommits)
		}
	}

	// Identity: the author and committer must be the ones this tool was told to use. An
	// environment variable can override git config, and a private identity must not ride along.
	want := strings.Join([]string{a.opts.AuthorName, a.opts.AuthorEmail, a.opts.AuthorName, a.opts.AuthorEmail}, "\n")
	if got, err := a.gitOut(b.Dir, "log", "-1", "--format=%an%n%ae%n%cn%n%ce"); err != nil {
		problem("%v", err)
	} else if strings.TrimSpace(got) != want {
		problem("the commit's author/committer is not %s <%s>", a.opts.AuthorName, a.opts.AuthorEmail)
	}

	// Tree half, over the committed tree.
	tarball, stderr, err := a.git.Run([]string{"git", "-C", b.Dir, "archive", "--format=tar", sha}, nil)
	if err != nil {
		problem("git archive of the new repository: %v: %s", err, strings.TrimSpace(string(stderr)))
		return
	}
	vdir, err := os.MkdirTemp(a.deps.TempDir, "leadkit-publish-verify-")
	if err != nil {
		problem("%v", err)
		return
	}
	defer os.RemoveAll(vdir)
	if err := extractTar(tarball, vdir); err != nil {
		problem("%v", err)
		return
	}
	findings, n, _, err := ScanTree(vdir, a.terms.Terms)
	if err != nil {
		problem("%v", err)
		return
	}
	b.TreeFindings, b.Files = findings, n
	committed, err := listTree(vdir)
	if err != nil {
		problem("%v", err)
		return
	}
	if missing, extra := diff(a.files, committed); len(missing)+len(extra) > 0 {
		problem("the committed tree is not the tree that was checked: %d file(s) missing%s, %d unexpected%s",
			len(missing), sample(missing), len(extra), sample(extra))
	}

	// History half, over every commit's message and identities, the files of every commit's tree
	// (those already searched before the build are not searched twice), and every ref name.
	hist, err := ScanHistory(a.git, b.Dir, []string{"--all"}, a.terms.Terms)
	if err != nil {
		problem("%v", err)
		return
	}
	b.HistoryFindings = hist.Findings
	if a.seenTrees == nil {
		a.seenTrees = map[string]bool{}
	}
	trees, err := ScanHistoryTrees(a.git, b.Dir, []string{"--all"}, a.terms.Terms, a.seenTrees)
	if err != nil {
		problem("%v", err)
		return
	}
	b.HistoryFindings = append(b.HistoryFindings, trees.Findings...)
	b.TreesSearched = len(a.seenTrees)
	refs, unreadable, err := ScanRefs(a.git, b.Dir, a.terms.Terms)
	if err != nil {
		problem("%v", err)
		return
	}
	b.HistoryFindings = append(b.HistoryFindings, refs...)
	for _, u := range unreadable {
		problem("the ref %s holds content this check does not read, so the history cannot be called clean", u)
	}
}

func diff(want, got []string) (missing, extra []string) {
	w, g := map[string]bool{}, map[string]bool{}
	for _, p := range want {
		w[p] = true
	}
	for _, p := range got {
		g[p] = true
		if !w[p] {
			extra = append(extra, p)
		}
	}
	for _, p := range want {
		if !g[p] {
			missing = append(missing, p)
		}
	}
	return missing, extra
}

func sample(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) > 3 {
		return " (e.g. " + strings.Join(paths[:3], ", ") + ")"
	}
	return " (" + strings.Join(paths, ", ") + ")"
}

// MatchBound states what a clean result does not cover, in every report, so a green is not read as
// more than it is. Closing it needs NFKC normalisation and case folding (golang.org/x/text), which
// leadkit does not depend on; the threat modelled is a name typed normally by accident.
const MatchBound = "matching is byte-for-byte after lowercasing; a leak spelled in another Unicode form " +
	"(decomposed accents, fullwidth letters, ligatures), inside a file that is not UTF-8 text " +
	"(UTF-16, compressed, an office document; only the CURRENT tree is listed as not searched as text, " +
	"not past releases), in the header of a merge commit that carries a signed tag (mergetag), encoded inside a text file (base64, percent-escaped), " +
	"absent from the term list, or a name that could not be listed because it is outside the term " +
	"alphabet (publish does not check those; check them by hand) will pass"

const maxListed = 40

// Write prints the report. It states the outcome per half, because a single CLEAN covers too much.
func (r Report) Write(w io.Writer) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "leadkit publish — %s (%s)\n", r.Ref, short(r.SHA))
	fmt.Fprintf(&b, "  terms: %d, from %s\n", r.TermCount, r.TermsSource)
	fmt.Fprintf(&b, "  bound: %s\n", MatchBound)
	fmt.Fprintf(&b, "  manifest: %s — %d path(s) excluded (%d file(s))\n", r.Manifest, len(r.Excluded), r.ExcludedFiles)
	for _, s := range r.Stale {
		fmt.Fprintf(&b, "  STALE: %s is in the manifest and no longer exists in the tree; fix the manifest\n", s)
	}
	if len(r.TreeFindings) == 0 {
		fmt.Fprintf(&b, "  tree: CLEAN — %d file(s) searched against %d term(s)\n", r.Files, r.TermCount)
	} else {
		fmt.Fprintf(&b, "  tree: FOUND %d match(es) in %d file(s) searched\n", len(r.TreeFindings), r.Files)
		list(&b, "    ", r.TreeFindings)
	}
	if len(r.NotText) == 0 {
		fmt.Fprintf(&b, "  not searched as text: none (every file is UTF-8 text)\n")
	} else {
		fmt.Fprintf(&b, "  not searched as text: %d file(s) are not UTF-8 text, so a term inside their encoded "+
			"content would not be seen (an ASCII term stored plainly still is):\n", len(r.NotText))
		for i, p := range r.NotText {
			if i == maxListed {
				fmt.Fprintf(&b, "    ... and %d more\n", len(r.NotText)-maxListed)
				break
			}
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	h := r.SourceHistory
	switch {
	case len(h.Findings) == 0:
		fmt.Fprintf(&b, "  source history: clean — %d commit(s) searched\n", h.Commits)
	case r.RequireCleanHistory:
		fmt.Fprintf(&b, "  source history: FOUND %d match(es) in %d of %d commit(s)\n", len(h.Findings), h.Affected(), h.Commits)
		list(&b, "    ", h.Findings)
	default:
		fmt.Fprintf(&b, "  source history: %d match(es) in %d of %d commit(s) — informational. "+
			"This is why the public repository is built as new commits and is never a fork or a filtered clone\n",
			len(h.Findings), h.Affected(), h.Commits)
	}
	if bt := r.Built; bt != nil {
		fmt.Fprintf(&b, "  built (%s): %s\n", bt.Mode, bt.Dir)
		if bt.Mode == "update" && bt.Commit != "" {
			fmt.Fprintf(&b, "    this release makes the public tree EQUAL the source tree: %d added, %d changed, %d removed\n",
				bt.Added, bt.Changed, len(bt.Removed))
		}
		if len(bt.Removed) > 0 {
			fmt.Fprintf(&b, "    WARNING: %d file(s) are in the public repository and NOT in the source, and this release deletes them. "+
				"A change made only on the public side (a merged contribution, a LICENSE, CI configuration) is reverted:\n", len(bt.Removed))
			for i, p := range bt.Removed {
				if i == maxListed {
					fmt.Fprintf(&b, "      ... and %d more\n", len(bt.Removed)-maxListed)
					break
				}
				fmt.Fprintf(&b, "      %s\n", p)
			}
		}
		switch {
		case bt.Commit == "":
			// Stopped before a commit existed: only a history match in the existing repository
			// can be shown.
			if len(bt.HistoryFindings) > 0 {
				fmt.Fprintf(&b, "    history: FOUND %d match(es) in the existing history\n", len(bt.HistoryFindings))
				list(&b, "      ", bt.HistoryFindings)
			}
		default:
			if len(bt.TreeFindings) == 0 {
				fmt.Fprintf(&b, "    tree: CLEAN in the committed tree (%d file(s))\n", bt.Files)
			} else {
				fmt.Fprintf(&b, "    tree: FOUND %d match(es) in the committed tree\n", len(bt.TreeFindings))
				list(&b, "      ", bt.TreeFindings)
			}
			if len(bt.HistoryFindings) == 0 {
				fmt.Fprintf(&b, "    history: CLEAN — %d commit(s): messages, identities, tag messages and taggers, ref names and the files of %d distinct tree(s)\n",
					bt.Commits, bt.TreesSearched)
			} else {
				fmt.Fprintf(&b, "    history: FOUND %d match(es)\n", len(bt.HistoryFindings))
				list(&b, "      ", bt.HistoryFindings)
			}
		}
		for _, p := range bt.Problems {
			fmt.Fprintf(&b, "    PROBLEM: %s\n", p)
		}
		if bt.Undone != "" {
			fmt.Fprintf(&b, "    undone: %s\n", bt.Undone)
		}
	}
	switch {
	case !r.OK():
		fmt.Fprintf(&b, "RESULT: NOT READY — nothing was published.\n")
	case r.Built != nil:
		fmt.Fprintf(&b, "RESULT: PREPARED, NOT PUBLISHED. Commit %s in %s.\n"+
			"  leadkit never creates a repository, adds a remote or pushes. Review it (git -C %s log --stat),\n"+
			"  then create or update the public repository and push it yourself.\n",
			short(r.Built.Commit), r.Built.Dir, r.Built.Dir)
	default:
		fmt.Fprintf(&b, "RESULT: CHECKED — the tree is clean against %d term(s). Nothing was built or published.\n", r.TermCount)
	}
	_, _ = w.Write(b.Bytes())
}

func list(b *bytes.Buffer, indent string, fs []Finding) {
	for i, f := range fs {
		if i == maxListed {
			fmt.Fprintf(b, "%s... and %d more\n", indent, len(fs)-maxListed)
			break
		}
		fmt.Fprintf(b, "%s%s\n", indent, f)
	}
}
