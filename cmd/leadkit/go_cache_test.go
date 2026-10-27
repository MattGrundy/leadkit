package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	goCacheOnce sync.Once
	goCacheDir  string
)

// realGoCache is the go build cache of the user running the suite, resolved once. Pinning HOME
// into a temp dir moves go's DEFAULT cache there too (macOS: $HOME/Library/Caches/go-build), so a
// CLI test that runs `go` (`test run` builds a fixture repo) would cold-build the standard library
// into its own sandbox: tens of megabytes and several seconds per test. t.TempDir removes that on
// a normal finish, but a run cut short (a timeout panic, a kill) never reaches the cleanup, so the
// cost must not be paid per sandbox. The cache is content-addressed, so sharing it leaks nothing
// between tests; the sandbox keeps leadkit off real state, not go off its own cache. Empty when it
// cannot be resolved, in which case go falls back to a cache in the sandbox HOME: slower, still
// correct.
func realGoCache() string {
	goCacheOnce.Do(func() {
		if gc := os.Getenv("GOCACHE"); gc != "" {
			goCacheDir = gc
			return
		}
		out, err := exec.Command("go", "env", "GOCACHE").Output()
		if err != nil {
			return
		}
		if gc := strings.TrimSpace(string(out)); filepath.IsAbs(gc) {
			goCacheDir = gc
		}
	})
	return goCacheDir
}

// TestSandboxedEnv_GoCacheIsNotInsideTheSandboxHome pins the cause of the per-test disk cost: the
// sandbox HOME must not also host the go build cache. An explicit GOCACHE in extraEnv still wins.
func TestSandboxedEnv_GoCacheIsNotInsideTheSandboxHome(t *testing.T) {
	home := t.TempDir()
	get := func(env []string) string {
		got := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GOCACHE="); ok {
				got = v
			}
		}
		return got
	}

	gc := get(sandboxedEnv(home, nil))
	if gc == "" {
		t.Fatal("sandboxedEnv left GOCACHE unset, so go defaults to a cache under the sandbox HOME")
	}
	if rel, err := filepath.Rel(home, gc); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("GOCACHE %q is inside the sandbox HOME %q", gc, home)
	}
	if want := "/explicit/cache"; get(sandboxedEnv(home, []string{"GOCACHE=" + want})) != want {
		t.Error("an explicit GOCACHE in extraEnv must override the default")
	}
}

// TestCLI_TestRun_LeavesNoBuildCacheInTheSandboxHome is the end-to-end check: `test run` builds a
// fixture repo, and a cold standard-library build inside the sandbox HOME is what makes a sandbox
// tens of megabytes.
func TestCLI_TestRun_LeavesNoBuildCacheInTheSandboxHome(t *testing.T) {
	bin := testBinary(t)
	repo, sha := gitFixtureRepo(t)
	home := t.TempDir()
	profilePath := filepath.Join(home, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "test", "run", "--repo", repo, "--sha", sha, "--affected=false")
	cmd.Dir = home
	cmd.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profilePath, "GOTELEMETRY=off"})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("test run: %v\n%s", err, out)
	}

	var total int64
	_ = filepath.Walk(home, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	const limit = 5 << 20
	if total > limit {
		t.Errorf("sandbox HOME holds %d bytes after `test run`; a go cache was built inside it", total)
	}
}
