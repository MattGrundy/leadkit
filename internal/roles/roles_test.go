package roles

import (
	"os"
	"path/filepath"
	"testing"
)

const fixtureROLES = `# comment lines and blanks are ignored

code-review     cr-1
code-review-2   cr-2
critique        crit-2
finalise        finalise
A prose line that happens to look tabular but is not a role mapping we'll ever query
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ROLES")
	if err := os.WriteFile(path, []byte(fixtureROLES), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolve(t *testing.T) {
	path := writeFixture(t)

	tests := map[string]struct {
		want string
		ok   bool
	}{
		"code-review":    {"cr-1", true},
		"critique":       {"crit-2", true},
		"finalise":       {"finalise", true},
		"does-not-exist": {"", false},
	}
	for role, want := range tests {
		got, ok := Resolve(path, role)
		if got != want.want || ok != want.ok {
			t.Errorf("Resolve(%q) = (%q, %v), want (%q, %v)", role, got, ok, want.want, want.ok)
		}
	}
}

func TestResolve_MissingFile(t *testing.T) {
	if _, ok := Resolve(filepath.Join(t.TempDir(), "does-not-exist"), "code-review"); ok {
		t.Fatal("want ok=false for a missing ROLES file")
	}
}
