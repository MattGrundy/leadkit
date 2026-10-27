package owners

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 235. The built-in block is what stops an edit to the file that grants merge authority, and it
// matched the path as written: a differently-cased spelling (the same file on APFS), a symlink alias of the
// directory, and a symlinked HOME all skipped it. Real directories and real symlinks below; the case rows
// fold the STRING, so they hold on a case-sensitive filesystem too.

type blockFixture struct {
	home, realHome, tmp string
}

func newBlockFixture(t *testing.T) blockFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := blockFixture{realHome: filepath.Join(root, "home"), tmp: filepath.Join(root, "elsewhere")}
	for _, d := range []string{filepath.Join(f.realHome, ".claude", "leadkit", "config"), filepath.Join(f.realHome, "other"), f.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	link(filepath.Join(f.realHome, ".claude", "leadkit", "config"), filepath.Join(f.tmp, "lnk")) // the directory itself
	link(filepath.Join(f.realHome, ".claude"), filepath.Join(f.tmp, "lnk2"))                     // an ancestor of it
	link(filepath.Join(f.realHome, "other"), filepath.Join(f.tmp, "lnk-other"))                  // somewhere harmless
	link(f.realHome, filepath.Join(root, "homelink"))                                            // HOME reached through a symlink
	f.home = f.realHome
	return f
}

func (f blockFixture) decide(tool, path, command, home string) Decision {
	return Decide(Input{Tool: tool, FilePath: path, Command: command, Caller: "lead", Home: home, Pwd: f.tmp, Resolve: ResolvePath})
}

func TestBuiltinBlock_EverySpellingOfTheConfigDirectoryIsRefused(t *testing.T) {
	f := newBlockFixture(t)
	cfg := filepath.Join(f.realHome, ".claude", "leadkit", "config")
	paths := map[string]string{
		"control":                           filepath.Join(cfg, "gated.yaml"),
		"cleaned dot-dot":                   cfg + "/../config/gated.yaml",
		"doubled slash":                     filepath.Join(f.realHome, ".claude", "leadkit") + "//config/gated.yaml",
		"case, whole path":                  strings.ToUpper(filepath.Join(cfg, "gated.yaml")),
		"case, home-relative part":          filepath.Join(f.realHome, ".Claude", "LeadKit", "Config", "gated.yaml"),
		"case, last directory":              filepath.Join(f.realHome, ".claude", "leadkit", "CONFIG", "gated.yaml"),
		"case, file name":                   filepath.Join(cfg, "GATED.YAML"),
		"symlink to the directory":          filepath.Join(f.tmp, "lnk", "gated.yaml"),
		"symlink to the directory, new sub": filepath.Join(f.tmp, "lnk", "sub", "deeper", "new.yaml"),
		"symlink to an ancestor":            filepath.Join(f.tmp, "lnk2", "leadkit", "config", "gated.yaml"),
		"symlink, name in other case":       filepath.Join(f.tmp, "lnk", "GATED.yaml"),
		"relative through the symlink":      "lnk/gated.yaml",
		"dot-dot after the symlink":         filepath.Join(f.tmp, "lnk") + "/../config/gated.yaml",
		"dot-dot after the symlink, parent": filepath.Join(f.tmp, "lnk2") + "/leadkit/../leadkit/config/gated.yaml",
		"relative up and through":           "../home/.claude/leadkit/config/gated.yaml",
	}
	homes := map[string]string{"real home": f.realHome, "home through a symlink": filepath.Join(filepath.Dir(f.realHome), "homelink")}
	for homeName, home := range homes {
		for name, p := range paths {
			for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
				if d := f.decide(tool, p, "", home); d.Allow {
					t.Errorf("%s / %s / %s: allowed %q (%s)", homeName, tool, name, p, d.Reason)
				}
			}
			for _, cmd := range []string{"rm '" + p + "'", "mv /dev/null '" + p + "'", "mv '" + p + "' /dev/null"} {
				if d := f.decide("Bash", "", cmd, home); d.Allow {
					t.Errorf("%s / Bash / %s: allowed %q (%s)", homeName, name, cmd, d.Reason)
				}
			}
		}
	}
	// The path spelled through a symlinked HOME while the guard was given the real one, and the reverse:
	// HOME and the path are resolved independently.
	via := filepath.Join(filepath.Dir(f.realHome), "homelink", ".claude", "leadkit", "config", "gated.yaml")
	if d := f.decide("Write", via, "", f.realHome); d.Allow {
		t.Errorf("a path spelled through a symlinked home, given the real home, was allowed: %s", d.Reason)
	}
}

// CONTROLS: the refusals above are about the config directory, not about case folding or symlinks in general.
func TestBuiltinBlock_NearbyPathsAreStillAllowed(t *testing.T) {
	f := newBlockFixture(t)
	for name, p := range map[string]string{
		"a file under the home":                 filepath.Join(f.realHome, "other", "x"),
		"a sibling under .claude":               filepath.Join(f.realHome, ".claude", "settings.json"),
		"a sibling of the config directory":     filepath.Join(f.realHome, ".claude", "leadkit", "state.json"),
		"a directory whose name starts with it": filepath.Join(f.realHome, ".claude", "leadkit", "configs", "x"),
		"a plain path elsewhere":                filepath.Join(f.tmp, "plain", "x"),
		"a symlink to somewhere harmless":       filepath.Join(f.tmp, "lnk-other", "x"),
		"another home's config directory":       filepath.Join(f.tmp, ".claude", "leadkit", "config", "x"),
	} {
		for _, home := range []string{f.realHome, filepath.Join(filepath.Dir(f.realHome), "homelink")} {
			if d := f.decide("Write", p, "", home); !d.Allow {
				t.Errorf("%s: refused %q (%s)", name, p, d.Reason)
			}
			if d := f.decide("Bash", "", "rm '"+p+"'", home); !d.Allow {
				t.Errorf("%s: refused rm of %q (%s)", name, p, d.Reason)
			}
		}
	}
}

// The escape hatch still lifts it, and a caller that is not the lead is refused the same.
func TestBuiltinBlock_BypassLiftsAndEveryCallerIsRefused(t *testing.T) {
	f := newBlockFixture(t)
	p := filepath.Join(f.tmp, "lnk", "gated.yaml")
	in := Input{Tool: "Write", FilePath: p, Caller: "lead", Home: f.realHome, Pwd: f.tmp, Resolve: ResolvePath}
	if d := Decide(in); d.Allow {
		t.Fatal("control: refused without the bypass")
	}
	in.Bypass = true
	if d := Decide(in); !d.Allow {
		t.Errorf("the bypass must lift the block: %s", d.Reason)
	}
	in.Bypass = false
	in.Caller = "dev-1"
	if d := Decide(in); d.Allow {
		t.Error("a non-lead caller must be refused too")
	}
	// With an owners policy loaded the built-in block still comes first.
	in.Policy = &Policy{}
	if d := Decide(in); d.Allow {
		t.Errorf("with a policy loaded the built-in block still applies: %s", d.Reason)
	}
	// Without a Resolve the guard sees lexical, case-folded paths only: the symlink alias is the documented
	// reason the caller must supply it.
	in.Policy, in.Caller, in.Resolve = nil, "lead", nil
	if d := Decide(in); !d.Allow {
		t.Errorf("without Resolve the symlink alias is not seen (this documents the dependency): %s", d.Reason)
	}
	in.FilePath = filepath.Join(f.realHome, ".CLAUDE", "leadkit", "config", "x")
	if d := Decide(in); d.Allow {
		t.Error("case folding needs no Resolve")
	}
}

func TestResolvePath(t *testing.T) {
	f := newBlockFixture(t)
	if got, want := ResolvePath(filepath.Join(f.tmp, "lnk", "a", "b")), filepath.Join(f.realHome, ".claude", "leadkit", "config", "a", "b"); got != want {
		t.Errorf("a path through a symlink, leaf not yet existing: %q, want %q", got, want)
	}
	if got := ResolvePath("relative/path"); got != "" {
		t.Errorf("a relative path is not resolved: %q", got)
	}
	if got, want := ResolvePath(filepath.Join(f.tmp, "lnk")), filepath.Join(f.realHome, ".claude", "leadkit", "config"); got != want {
		t.Errorf("the symlink itself: %q, want %q", got, want)
	}
}

// The pattern is matched case-folded too, so a mixed-case entry in the list still blocks. (The shipped
// pattern is lower case; this is the only thing that fails if the folding of the pattern is dropped.)
func TestBuiltinBlock_ThePatternIsFoldedToo(t *testing.T) {
	f := newBlockFixture(t)
	saved := BuiltinHardBlock
	t.Cleanup(func() { BuiltinHardBlock = saved })
	BuiltinHardBlock = []string{".Claude/LeadKit/Config/*"}
	if d := f.decide("Write", filepath.Join(f.realHome, ".claude", "leadkit", "config", "x"), "", f.realHome); d.Allow {
		t.Errorf("a mixed-case pattern must still block the lower-case spelling: %s", d.Reason)
	}
}
