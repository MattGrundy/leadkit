package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// `git log --all` shows commits, so a tag's message and tagger are invisible to it, and `--tags`
// publishes them. Each of these printed `history: CLEAN` and PREPARED with the term in a tag.
func TestPrepare_UpdateSearchesTheTagsOfThePublicRepository(t *testing.T) {
	hermetic(t)
	for name, tag := range map[string][]string{
		"message":      {"tag", "-a", "-m", "released for " + fixtureTerm, "v1"},
		"tagger name":  {"-c", "user.name=dev " + fixtureTerm, "tag", "-a", "-m", "release", "v1"},
		"tagger email": {"-c", "user.email=" + fixtureTerm + "@example.test", "tag", "-a", "-m", "release", "v1"},
		"ref name":     {"tag", fixtureTerm + "-v1"},
	} {
		repo := source(t, cleanFiles(), "msg")
		pub := publicClone(t, repo)
		git(t, pub, tag...)
		head := git(t, pub, "rev-parse", "HEAD")
		nextRelease(t, repo, map[string]string{"added.go": "package added\n"})

		rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b := rep.Built
		if rep.OK() || b == nil || len(b.HistoryFindings) == 0 {
			t.Errorf("%s: a term in a tag must refuse: %+v", name, b)
			continue
		}
		if b.Commit != "" || b.Undone != "" || git(t, pub, "rev-parse", "HEAD") != head {
			t.Errorf("%s: the refusal comes before anything is written: %+v", name, b)
		}
		if w := b.HistoryFindings[0].Where; !strings.HasPrefix(w, "tag refs/tags/v1") && !strings.HasPrefix(w, "ref name ") {
			t.Errorf("%s: the finding must name the tag: %v", name, b.HistoryFindings[0])
		}
	}

	// Controls: a harmless annotated tag and a lightweight one proceed, and the report says tags
	// were covered.
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	git(t, pub, "tag", "-a", "-m", "release one", "v1")
	git(t, pub, "tag", "light")
	nextRelease(t, repo, map[string]string{"added.go": "package added\n"})
	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || !rep.OK() {
		t.Fatalf("control: %v %+v", err, rep.Built)
	}
	var out strings.Builder
	rep.Write(&out)
	if !strings.Contains(out.String(), "tag messages and taggers") {
		t.Errorf("the history line must say tags were covered:\n%s", out.String())
	}
}

// A ref at a blob or a tree, or a tag of anything but a commit, holds content this check does not
// read. It is refused rather than passed over.
func TestPrepare_UpdateRefusesRefsItCannotRead(t *testing.T) {
	hermetic(t)
	for name, make := range map[string]func(t *testing.T, pub string){
		"ref at a blob": func(t *testing.T, pub string) {
			git(t, pub, "update-ref", "refs/notes/blobref", git(t, pub, "rev-parse", "HEAD:README.md"))
		},
		"ref at a tree": func(t *testing.T, pub string) {
			git(t, pub, "update-ref", "refs/notes/treeref", git(t, pub, "rev-parse", "HEAD^{tree}"))
		},
		"tag of a tag": func(t *testing.T, pub string) {
			git(t, pub, "tag", "-a", "-m", "inner", "inner")
			git(t, pub, "tag", "-a", "-m", "outer", "outer", "refs/tags/inner")
		},
		"tag of a blob": func(t *testing.T, pub string) {
			git(t, pub, "tag", "-a", "-m", "on a blob", "blobtag", git(t, pub, "rev-parse", "HEAD:README.md"))
		},
	} {
		repo := source(t, cleanFiles(), "msg")
		pub := publicClone(t, repo)
		make(t, pub)
		head := git(t, pub, "rev-parse", "HEAD")
		nextRelease(t, repo, map[string]string{"added.go": "package added\n"})

		rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.OK() || rep.Built == nil || !strings.Contains(strings.Join(rep.Built.Problems, "\n"), "does not read") {
			t.Errorf("%s: want the unreadable ref refused: %+v", name, rep.Built)
			continue
		}
		if rep.Built.Commit != "" || git(t, pub, "rev-parse", "HEAD") != head {
			t.Errorf("%s: nothing may be written: %+v", name, rep.Built)
		}
	}
}

// refsAs answers every `for-each-ref` with a fixed listing, standing in for a repository whose
// refs differ from what the build assumed. Everything else passes through.
type refsAs struct {
	Runner
	listing string
}

func (r refsAs) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	for _, a := range argv {
		if a == "for-each-ref" {
			return []byte(r.listing), nil, nil
		}
	}
	return r.Runner.Run(argv, stdin)
}

// The re-check inside the new repository reads refs too, not only the pre-write one.
func TestPrepare_TheRecheckInsideTheNewRepositoryReadsRefs(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	for name, tc := range map[string]struct{ listing, want string }{
		"a tag message":     {"\x1erefs/tags/v1\x1ftag\x1fcommit\x1f\x1f\x1freleased for " + fixtureTerm + "\n", "tag refs/tags/v1"},
		"an unreadable ref": {"\x1erefs/notes/x\x1fblob\x1f\x1f\x1f\x1f\n", ""},
	} {
		d := deps(t, nil)
		d.Git = refsAs{Runner: d.Git, listing: tc.listing}
		out := filepath.Join(t.TempDir(), "public")
		rep, err := Prepare(Options{Repo: repo, Out: out}, d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b := rep.Built
		if rep.OK() || b == nil {
			t.Errorf("%s: must fail the re-check: %+v", name, b)
			continue
		}
		if tc.want != "" && (len(b.HistoryFindings) == 0 || !strings.HasPrefix(b.HistoryFindings[0].Where, tc.want)) {
			t.Errorf("%s: want a finding for %q: %+v", name, tc.want, b.HistoryFindings)
		}
		if tc.want == "" && !strings.Contains(strings.Join(b.Problems, "\n"), "does not read") {
			t.Errorf("%s: want the unreadable ref named: %+v", name, b.Problems)
		}
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the failed repository must be removed", name)
		}
	}
}

// The listing is framed with two bytes a tag message can contain, so a message can forge a record.
// The reviewer's exact forgery names a record of type commit, whose text is never read: the term
// after it was never searched and the run printed CLEAN.
func TestPrepare_ATagMessageCannotForgeARecord(t *testing.T) {
	hermetic(t)
	for name, msg := range map[string]string{
		// The reviewer's message: the forged record is not a ref at all.
		"not a ref": "ok\x1eX\x1fcommit\x1f\x1f\x1f\x1f hidden " + fixtureTerm,
		// A careful one: it borrows a ref name, so only a search of the whole listing finds it.
		"borrowed ref": "ok\x1erefs/heads/zz\x1fcommit\x1f\x1f\x1f\x1f\x1fhidden " + fixtureTerm,
	} {
		repo := source(t, cleanFiles(), "msg")
		pub := publicClone(t, repo)
		git(t, pub, "tag", "-a", "-m", msg, "v1")
		head := git(t, pub, "rev-parse", "HEAD")
		nextRelease(t, repo, map[string]string{"added.go": "package added\n"})

		rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
		refused := err != nil || (!rep.OK() && rep.Built != nil && len(rep.Built.HistoryFindings) > 0)
		if !refused {
			t.Errorf("%s: a forged record must not read as clean: err=%v %+v", name, err, rep.Built)
		}
		if name == "not a ref" && (err == nil || !strings.Contains(err.Error(), "not a ref")) {
			t.Errorf("%s: want the listing refused as untrustworthy: %v", name, err)
		}
		if git(t, pub, "rev-parse", "HEAD") != head {
			t.Errorf("%s: nothing may be written", name)
		}
	}
}

func TestScanRefs_ReadsTheWholeListingNotJustItsRecords(t *testing.T) {
	terms, err := scrubterms.Parse(termFileText)
	if err != nil {
		t.Fatal(err)
	}
	// Control: a well-formed listing with no term is clean.
	clean := "\x1erefs/heads/main\x1fcommit\x1f\x1f\x1f\x1f msg\n\x1erefs/tags/v1\x1ftag\x1fcommit\x1ftagger\x1ft@example.test\x1fnotes\n"
	if f, u, err := ScanRefs(&fakeGH{body: clean}, "/r", terms); err != nil || len(f) != 0 || len(u) != 0 {
		t.Fatalf("control: %v %v %v", f, u, err)
	}
	forged := "\x1erefs/tags/v1\x1ftag\x1fcommit\x1ft\x1ft@example.test\x1fok\x1erefs/heads/zz\x1fcommit\x1f\x1f\x1f\x1fhidden " + fixtureTerm + "\n"
	// An unreadable ref does not switch the whole-listing search off.
	withBlob := "\x1erefs/notes/b\x1fblob\x1f\x1f\x1f\x1f\n" + forged
	if f, u, err := ScanRefs(&fakeGH{body: withBlob}, "/r", terms); err != nil || len(u) != 1 || len(f) == 0 {
		t.Fatalf("both the unreadable ref and the hidden term must be reported: %v %v %v", f, u, err)
	}
	f, _, err := ScanRefs(&fakeGH{body: forged}, "/r", terms)
	if err != nil || len(f) == 0 || f[0].Where != "the refs listing" {
		t.Fatalf("the term after a forged record must still be found: %v %v", f, err)
	}
}
