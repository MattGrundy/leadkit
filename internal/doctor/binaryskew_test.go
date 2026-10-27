package doctor

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFileInfo is a minimal os.FileInfo for the injected Stat.
type fakeFileInfo struct{ dir bool }

func (f fakeFileInfo) Name() string       { return "" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() interface{}   { return nil }

// fakeFS builds injectable Stat + EvalSymlinks from a map of path -> real path.
// A path present in the map "exists"; its value is its resolved real path.
func fakeFS(real map[string]string) (func(string) (os.FileInfo, error), func(string) (string, error)) {
	stat := func(p string) (os.FileInfo, error) {
		if _, ok := real[p]; ok {
			return fakeFileInfo{dir: false}, nil
		}
		return nil, &fs.PathError{Op: "stat", Path: p, Err: os.ErrNotExist}
	}
	eval := func(p string) (string, error) {
		if r, ok := real[p]; ok {
			return r, nil
		}
		return "", &fs.PathError{Op: "eval", Path: p, Err: os.ErrNotExist}
	}
	return stat, eval
}

func TestBinarySkew_StaleFirstOnPathWarns(t *testing.T) {
	// The #123 trap: ~/go/bin (a stale old build) precedes ~/.local/bin on PATH, so the
	// shell runs the stale one. They resolve to DIFFERENT real files.
	stalePath := "/home/u/go/bin/leadkit"
	deployedPath := "/home/u/.local/bin/leadkit"
	stat, eval := fakeFS(map[string]string{
		stalePath:    "/home/u/go/bin/leadkit",     // stale real file
		deployedPath: "/home/u/.local/bin/leadkit", // current real file
	})
	got := binarySkewCheck(BinarySkewInput{
		PathEnv:      "/home/u/go/bin" + string(os.PathListSeparator) + "/home/u/.local/bin",
		Self:         deployedPath, // doctor runs the deployed one
		EvalSymlinks: eval,
		Stat:         stat,
	})
	if got.OK || got.Severity != SeverityWarn {
		t.Fatalf("want a WARN (OK=false, severity=warn), got OK=%v severity=%s", got.OK, got.Severity)
	}
	for _, want := range []string{stalePath, deployedPath, "first on PATH", "WINS", "Remedy", "PATH order", "escapes governance"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("WARN detail must contain %q; got:\n%s", want, got.Detail)
		}
	}
	// Never a hard error.
	if got.Severity == SeverityCritical {
		t.Error("binary-skew must never be critical")
	}
}

func TestBinarySkew_SingleBinaryOK(t *testing.T) {
	p := "/home/u/.local/bin/leadkit"
	stat, eval := fakeFS(map[string]string{p: p})
	got := binarySkewCheck(BinarySkewInput{
		PathEnv:      "/home/u/.local/bin" + string(os.PathListSeparator) + "/usr/bin",
		Self:         p,
		EvalSymlinks: eval,
		Stat:         stat,
	})
	if !got.OK || got.Severity != SeverityInfo {
		t.Fatalf("single leadkit should be OK/Info, got OK=%v severity=%s detail=%q", got.OK, got.Severity, got.Detail)
	}
}

func TestBinarySkew_AllSameRealFileOK(t *testing.T) {
	// Two PATH entries + a symlink all resolving to ONE real file — no skew.
	linkA := "/home/u/.local/bin/leadkit"
	linkB := "/home/u/bin/leadkit"
	realFile := "/opt/leadkit/leadkit"
	stat, eval := fakeFS(map[string]string{
		linkA: realFile,
		linkB: realFile,
	})
	got := binarySkewCheck(BinarySkewInput{
		PathEnv:      "/home/u/.local/bin" + string(os.PathListSeparator) + "/home/u/bin",
		Self:         realFile,
		EvalSymlinks: eval,
		Stat:         stat,
	})
	if !got.OK || got.Severity != SeverityInfo {
		t.Fatalf("all-same-real-file should be OK/Info, got OK=%v severity=%s detail=%q", got.OK, got.Severity, got.Detail)
	}
	if !strings.Contains(got.Detail, realFile) {
		t.Errorf("detail should name the shared real file %q; got %q", realFile, got.Detail)
	}
}

func TestBinarySkew_NoBinaryOK(t *testing.T) {
	stat, eval := fakeFS(map[string]string{})
	got := binarySkewCheck(BinarySkewInput{
		PathEnv:      "/usr/bin" + string(os.PathListSeparator) + "/bin",
		EvalSymlinks: eval,
		Stat:         stat,
	})
	if !got.OK || got.Severity != SeverityInfo {
		t.Fatalf("no binary found should be OK/Info, got OK=%v severity=%s", got.OK, got.Severity)
	}
}

func TestBinarySkew_OffPathExtraStillSurfaced(t *testing.T) {
	// The deployed one is on PATH; a stale one sits at ~/go/bin/leadkit which is NOT on
	// PATH but is a known extra candidate — still a latent trap → WARN.
	deployed := "/home/u/.local/bin/leadkit"
	staleExtra := filepath.Join("/home/u/go/bin/leadkit")
	stat, eval := fakeFS(map[string]string{
		deployed:   deployed,
		staleExtra: staleExtra,
	})
	got := binarySkewCheck(BinarySkewInput{
		PathEnv:         "/home/u/.local/bin",
		ExtraCandidates: []string{staleExtra},
		Self:            deployed,
		EvalSymlinks:    eval,
		Stat:            stat,
	})
	if got.OK || got.Severity != SeverityWarn {
		t.Fatalf("off-PATH stale extra should WARN, got OK=%v severity=%s detail=%q", got.OK, got.Severity, got.Detail)
	}
	if !strings.Contains(got.Detail, "off-PATH") {
		t.Errorf("detail should mark the extra as off-PATH; got %q", got.Detail)
	}
}
