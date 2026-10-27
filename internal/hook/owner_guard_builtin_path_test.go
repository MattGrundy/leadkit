package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// Ticket 235 through the hook: the built-in block on the account's config directory holds against a case
// variant and a symlink alias, which needs the filesystem resolver the wrapper (and only the wrapper)
// supplies to the pure decision core.
func TestOwnerGuard_TheBuiltinBlockSeesCaseVariantsAndSymlinkAliases(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	cfg := filepath.Join(home, ".claude", "leadkit", "config")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(cfg, alias); err != nil {
		t.Fatal(err)
	}
	guard := func(path string) Result {
		return OwnerGuard(Input{ToolName: "Write", ToolInput: toolInput{FilePath: path}}, OwnerGuardConfig{Caller: "lead", Home: home, Pwd: root})
	}
	for name, path := range map[string]string{
		"control":       filepath.Join(cfg, "gated.yaml"),
		"case variant":  filepath.Join(home, ".Claude", "LEADKIT", "Config", "gated.yaml"),
		"symlink alias": filepath.Join(alias, "gated.yaml"),
	} {
		if res := guard(path); res.ExitCode != 2 {
			t.Errorf("%s: %q was allowed: %+v", name, path, res)
		}
	}
	if res := guard(filepath.Join(home, "other", "x")); res.ExitCode != 0 {
		t.Errorf("control: an unrelated path must be allowed, got %+v", res)
	}
}
