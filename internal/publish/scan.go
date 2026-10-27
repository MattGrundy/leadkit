package publish

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// Finding is one place a forbidden term appears.
type Finding struct {
	// Where is a file path, "path of <file>" for a term in a name, or "commit <sha>".
	Where string
	// Line is 1-based, or 0 when the term is in a name rather than in content.
	Line int
	Term string
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.Where, f.Line, f.Term)
	}
	return fmt.Sprintf("%s: %s", f.Where, f.Term)
}

// ScanTree searches every file under root for the terms and returns the findings and the number
// of files and links examined. There is NO extension filter, deliberately: the repo's own
// ratchets cover Go and prose, so a term in a shell script, a workflow, a Makefile or a binary
// passes them. A term in a file or directory NAME is a finding too, and so is one in a symlink's
// target, because a committed symlink into someone's home directory is how an absolute path
// has shipped before.
//
// A "src:" term is not matched inside a _test.go file, which legitimately reuses it as a
// placeholder, and is a finding anywhere else.
//
// opaque lists the files that are not UTF-8 text (a NUL byte or invalid UTF-8: UTF-16, compressed
// or office documents, images). They are still searched as raw bytes, which finds an ASCII term
// stored plainly, but a term inside the encoded content is NOT seen, so each run names them rather
// than leaving the gap to a sentence in the docs.
func ScanTree(root string, terms scrubterms.Terms) ([]Finding, int, []string, error) {
	var out []Finding
	var opaque []string
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		use := terms.Everywhere()
		if strings.HasSuffix(rel, "_test.go") {
			use = terms.ForTests()
		}
		low := strings.ToLower(rel)
		for _, t := range use {
			if strings.Contains(low, t) {
				out = append(out, Finding{Where: "path of " + rel, Term: t})
			}
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			files++
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			out = append(out, scanText(rel+" (symlink target)", target, use)...)
		case d.Type().IsRegular():
			files++
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
				opaque = append(opaque, rel)
			}
			out = append(out, scanBytes(rel, data, use)...)
		}
		return nil
	})
	return out, files, opaque, err
}

func scanText(where, text string, terms []string) []Finding {
	return scanBytes(where, []byte(text), terms)
}

// scanBytes reports each line that holds a term, case-insensitively.
func scanBytes(where string, data []byte, terms []string) []Finding {
	low := bytes.ToLower(data)
	var hit []string
	for _, t := range terms {
		if bytes.Contains(low, []byte(t)) {
			hit = append(hit, t)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	var out []Finding
	for i, line := range bytes.Split(low, []byte("\n")) {
		for _, t := range hit {
			if bytes.Contains(line, []byte(t)) {
				out = append(out, Finding{Where: where, Line: i + 1, Term: t})
			}
		}
	}
	return out
}

// HistoryResult is what a search of commit messages and identities found.
type HistoryResult struct {
	Commits  int // commits examined
	Trees    int // distinct trees whose files were searched (ScanHistoryTrees only)
	Findings []Finding
}

// Affected is the number of distinct commits with at least one finding.
func (h HistoryResult) Affected() int {
	seen := map[string]bool{}
	for _, f := range h.Findings {
		seen[f.Where] = true
	}
	return len(seen)
}

// recordSep separates commits in the log format below. It is the ASCII record separator, which
// a commit message cannot sensibly contain.
const recordSep = "\x1e"

// ScanHistory searches the messages, author names and addresses, and committer names and
// addresses of the commits reachable from revs. `git grep` searches trees and never messages, so
// this is the half of the check that a tree scan cannot do.
func ScanHistory(git Runner, repo string, revs []string, terms scrubterms.Terms) (HistoryResult, error) {
	argv := append([]string{"git", "-C", repo, "log", "--format=" + recordSep + "%H%n%an%n%ae%n%cn%n%ce%n%B"}, revs...)
	stdout, stderr, err := git.Run(argv, nil)
	if err != nil {
		return HistoryResult{}, fmt.Errorf("git log: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var res HistoryResult
	for _, rec := range strings.Split(string(stdout), recordSep) {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		res.Commits++
		sha, body, _ := strings.Cut(rec, "\n")
		res.Findings = append(res.Findings, scanText("commit "+short(sha), body, terms.Everywhere())...)
	}
	return res, nil
}

func short(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ScanHistoryTrees searches the FILES of every distinct tree reachable from revs, not just the
// messages. A delta release can never remove a blob from an earlier commit, so a term that
// shipped in release 1 before it was on the list stays findable (git grep on that commit) for as
// long as the repository exists; a check of the current tree and the commit messages alone would
// report that repository clean. Trees already in seen are skipped, and the ones searched are
// added to it, so a caller can scan the existing history once and the new commit after.
//
// The question is "what blobs exist in this history", so the trees are read with ls-tree and
// cat-file, which apply NO attributes. `git archive` answers "what would be exported": it honours
// the tree's own .gitattributes, so a file marked export-ignore is omitted from it and was never
// searched, while really existing in public history. Names, blob contents and symlink targets
// (a mode-120000 blob's content) are searched as ScanTree searches a directory: no extension
// filter, and a src: term is not matched in _test.go files. Findings are named
// "commit <sha> · <path>"; a leak that persists across many commits is reported once per
// distinct tree that holds it.
func ScanHistoryTrees(git Runner, repo string, revs []string, terms scrubterms.Terms, seen map[string]bool) (HistoryResult, error) {
	argv := append([]string{"git", "-C", repo, "log", "--format=%H %T"}, revs...)
	stdout, stderr, err := git.Run(argv, nil)
	if err != nil {
		return HistoryResult{}, fmt.Errorf("git log: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	var res HistoryResult
	// Blob findings are cached by object id and whether the path is a test file, because the same
	// blob sits in many trees and is read once.
	type blobKey struct {
		sha  string
		test bool
	}
	cache := map[blobKey][]Finding{}
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		commit, tree, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || tree == "" {
			continue
		}
		res.Commits++
		if seen[tree] {
			continue
		}
		seen[tree] = true
		res.Trees++

		listing, errb, err := git.Run([]string{"git", "-C", repo, "ls-tree", "-r", "-z", tree}, nil)
		if err != nil {
			return HistoryResult{}, fmt.Errorf("git ls-tree %s: %w: %s", short(tree), err, strings.TrimSpace(string(errb)))
		}
		type entry struct{ sha, path string }
		var blobs []entry
		var want []string
		wantSeen := map[string]bool{}
		for _, rec := range strings.Split(string(listing), "\x00") {
			meta, path, ok := strings.Cut(rec, "\t")
			if !ok {
				continue
			}
			f := strings.Fields(meta) // mode type sha
			if len(f) != 3 || f[1] != "blob" {
				continue // a submodule commit has no content here
			}
			e := entry{sha: f[2], path: path}
			if f[0] == "120000" {
				e.path = path + "\x00link" // marks a symlink; the content is its target
			}
			blobs = append(blobs, e)
			if k := (blobKey{f[2], strings.HasSuffix(path, "_test.go")}); !wantSeen[f[2]] {
				if _, done := cache[k]; !done {
					wantSeen[f[2]] = true
					want = append(want, f[2])
				}
			}
		}
		contents, err := readBlobs(git, repo, want)
		if err != nil {
			return HistoryResult{}, err
		}
		prefix := "commit " + short(commit) + " · "
		for _, e := range blobs {
			path, link := strings.CutSuffix(e.path, "\x00link")
			use := terms.Everywhere()
			isTest := strings.HasSuffix(path, "_test.go")
			if isTest {
				use = terms.ForTests()
			}
			for _, t := range use {
				if strings.Contains(strings.ToLower(path), t) {
					res.Findings = append(res.Findings, Finding{Where: prefix + "path of " + path, Term: t})
				}
			}
			k := blobKey{e.sha, isTest}
			if _, done := cache[k]; !done {
				cache[k] = scanBytes("", contents[e.sha], use)
			}
			where := prefix + path
			if link {
				where += " (symlink target)"
			}
			for _, f := range cache[k] {
				f.Where = where
				res.Findings = append(res.Findings, f)
			}
		}
	}
	return res, nil
}

// readBlobs returns the content of each object id through one `cat-file --batch`. An object the
// repository does not have is an error: a history that cannot be read cannot be called clean.
func readBlobs(git Runner, repo string, shas []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(shas) == 0 {
		return out, nil
	}
	stdout, stderr, err := git.Run([]string{"git", "-C", repo, "cat-file", "--batch"}, []byte(strings.Join(shas, "\n")+"\n"))
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	r := bufio.NewReader(bytes.NewReader(stdout))
	for _, want := range shas {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: short output reading %s: %w", short(want), err)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != want || f[1] != "blob" {
			return nil, fmt.Errorf("git cat-file: cannot read %s: %q", short(want), strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("git cat-file: bad size in %q", strings.TrimSpace(header))
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("git cat-file: short content for %s: %w", short(want), err)
		}
		if _, err := r.ReadByte(); err != nil { // the newline after each object
			return nil, fmt.Errorf("git cat-file: missing terminator after %s: %w", short(want), err)
		}
		out[want] = body
	}
	return out, nil
}

// refFormat is the one for-each-ref format the guard permits: records start at 0x1e, fields are
// separated by 0x1f. Fields: ref name; its object type; the type an annotated tag points at
// DIRECTLY (a tag field, empty otherwise, and unlike *objecttype it does not peel a chain of
// tags); tagger name; tagger address; the full message.
const refFormat = "%1e%(refname)%1f%(objecttype)%1f%(type)%1f%(taggername)%1f%(taggeremail)%1f%(contents)"

// ScanRefs searches what `git log` cannot see: the names of every ref, and the message, tagger
// name and tagger address of every annotated tag (git log --all lists commits only, and `--tags`
// publishes tags). A ref that points at a blob or a tree, or an annotated tag that points at
// anything but a commit (another tag, a blob, a tree), holds content this check does not read,
// so it is returned in unreadable instead of being passed over as clean.
func ScanRefs(git Runner, repo string, terms scrubterms.Terms) (findings []Finding, unreadable []string, err error) {
	stdout, stderr, err := git.Run([]string{"git", "-C", repo, "for-each-ref", "--format=" + refFormat}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("git for-each-ref: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	use := terms.Everywhere()
	for _, rec := range strings.Split(string(stdout), "\x1e") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 6)
		if len(f) != 6 {
			return nil, nil, fmt.Errorf("git for-each-ref: cannot read the record %q", rec)
		}
		// Every genuine record starts with a ref name. A tag message may contain the separator
		// bytes and so forge a record boundary; one whose first field is not a ref refuses the
		// whole listing instead of being read as a ref.
		if !strings.HasPrefix(f[0], "refs/") {
			return nil, nil, fmt.Errorf("git for-each-ref: a record that is not a ref (%q); a ref message may contain the "+
				"record separator, so this listing cannot be trusted and nothing was checked", short(f[0]))
		}
		name, typ, target, taggerName, taggerEmail, contents := f[0], f[1], f[2], f[3], f[4], f[5]
		findings = append(findings, scanText("ref name "+name, name, use)...)
		switch {
		case typ == "commit":
			// Its message is searched with the commits.
		case typ == "tag" && target == "commit":
			findings = append(findings, scanText("tag "+name+" (message)", contents, use)...)
			findings = append(findings, scanText("tag "+name+" (tagger name)", taggerName, use)...)
			findings = append(findings, scanText("tag "+name+" (tagger address)", taggerEmail, use)...)
		default:
			what := typ
			if typ == "tag" {
				what = "a tag of a " + target
			}
			unreadable = append(unreadable, name+" ("+what+")")
		}
	}
	// The separators are bytes a message can contain, so the framing above is not proof of what
	// is a ref and what is message text. The terms are searched in the WHOLE listing as well, so
	// a forged boundary cannot hide text from the search; this only adds findings when the parsed
	// ones found none, to avoid reporting the same text twice.
	if len(findings) == 0 {
		findings = append(findings, scanText("the refs listing", string(stdout), use)...)
	}
	return findings, unreadable, nil
}
