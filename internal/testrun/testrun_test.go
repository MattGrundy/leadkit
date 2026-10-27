package testrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubRun installs a fake runFunc for the duration of the test — real subprocess
// execution never happens in this package's own tests (per the build task's own
// safety requirement).
func stubRun(t *testing.T, fn func(dir string, timeout time.Duration, command string) (string, int, error)) {
	t.Helper()
	orig := runFunc
	runFunc = fn
	t.Cleanup(func() { runFunc = orig })
}

func TestRun_ShaMismatchRefuses(t *testing.T) {
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", 0, nil
		}
		t.Fatalf("should never reach the test-suite command on a sha mismatch, got %q", command)
		return "", 0, nil
	})
	_, err := Run(Options{Repo: "/repo", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if err == nil {
		t.Fatal("want an error when HEAD doesn't match the requested sha")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v, want it to mention refusing", err)
	}
}

func TestRun_GoRepoInfersFreshGoTest(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var ranCommand string
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		ranCommand = command
		return "ok\n", 0, nil
	})
	v, err := Run(Options{Repo: repo, SHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	if ranCommand != "go test ./... -count=1" {
		t.Errorf("ranCommand = %q, want the fresh go test invocation", ranCommand)
	}
	if v.Verdict != "green" {
		t.Errorf("Verdict = %q, want green", v.Verdict)
	}
}

func TestRun_NodeRepoInfersTestScriptPerLockfile(t *testing.T) {
	cases := []struct {
		lockfile string
		want     string
	}{
		{"pnpm-lock.yaml", "pnpm test"},
		{"yarn.lock", "yarn test"},
		{"package-lock.json", "npm test"},
	}
	for _, c := range cases {
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, c.lockfile), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		var ranCommand string
		stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
			if command == "git rev-parse HEAD" {
				return "sha1\n", 0, nil
			}
			ranCommand = command
			return "ok\n", 0, nil
		})
		if _, err := Run(Options{Repo: repo, SHA: "sha1"}); err != nil {
			t.Fatal(err)
		}
		if ranCommand != c.want {
			t.Errorf("lockfile %s: ranCommand = %q, want %q", c.lockfile, ranCommand, c.want)
		}
	}
}

func TestRun_ProfileOverrideWinsOverInference(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var ranCommand string
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		ranCommand = command
		return "ok\n", 0, nil
	})
	if _, err := Run(Options{Repo: repo, SHA: "sha1", TestCommand: "make test-fast"}); err != nil {
		t.Fatal(err)
	}
	if ranCommand != "make test-fast" {
		t.Errorf("ranCommand = %q, want the profile override, not the go.mod inference", ranCommand)
	}
}

func TestRun_AffectedCommandOverrideWinsWhenAffectedRequested(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var ranCommand string
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		ranCommand = command
		return "ok\n", 0, nil
	})
	if _, err := Run(Options{Repo: repo, SHA: "sha1", Affected: true, AffectedCommand: "custom-affected-runner"}); err != nil {
		t.Fatal(err)
	}
	if ranCommand != "custom-affected-runner" {
		t.Errorf("ranCommand = %q, want the affected override", ranCommand)
	}
}

func TestRun_GreenVerdict(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644)
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		return "ok\nPASS\n", 0, nil
	})
	v, err := Run(Options{Repo: repo, SHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "green" || len(v.Failing) != 0 {
		t.Errorf("v = %+v, want green with no failing", v)
	}
}

func TestRun_RedVerdictParsesGoFailingTests(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644)
	output := "=== RUN   TestFoo\n--- FAIL: TestFoo (0.01s)\n    foo_test.go:10: boom\n--- FAIL: TestBar (0.02s)\nFAIL\n"
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		return output, 1, nil
	})
	v, err := Run(Options{Repo: repo, SHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "red" {
		t.Fatalf("Verdict = %q, want red", v.Verdict)
	}
	if len(v.Failing) != 2 || v.Failing[0] != "TestFoo" || v.Failing[1] != "TestBar" {
		t.Errorf("Failing = %v, want [TestFoo TestBar]", v.Failing)
	}
}

func TestRun_RedVerdictParsesJestVitestFailures(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o644)
	output := "FAIL src/foo.test.ts\n  ✕ renders correctly (12 ms)\n  ✓ does something else\nTests: 1 failed, 1 passed\n"
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		return output, 1, nil
	})
	v, err := Run(Options{Repo: repo, SHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "red" {
		t.Fatalf("Verdict = %q, want red", v.Verdict)
	}
	foundSuite, foundCase := false, false
	for _, f := range v.Failing {
		if f == "src/foo.test.ts" {
			foundSuite = true
		}
		if f == "renders correctly" {
			foundCase = true
		}
	}
	if !foundSuite || !foundCase {
		t.Errorf("Failing = %v, want both the suite path and the failing case name", v.Failing)
	}
}

func TestRun_JSONShape(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o644)
	stubRun(t, func(dir string, timeout time.Duration, command string) (string, int, error) {
		if command == "git rev-parse HEAD" {
			return "sha1\n", 0, nil
		}
		return "--- FAIL: TestX\n", 1, nil
	})
	v, err := Run(Options{Repo: repo, SHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.SHA != "sha1" || v.Verdict != "red" || len(v.Failing) != 1 || v.Failing[0] != "TestX" {
		t.Errorf("v = %+v, want the full {sha, verdict, failing} shape populated", v)
	}
}

func TestFormatVerdict_GreenAndRed(t *testing.T) {
	green := FormatVerdict(Verdict{SHA: "abcdefabcdefabcdef", Verdict: "green"})
	if !strings.Contains(green, "tests: green @abcdefabcdef") {
		t.Errorf("green format = %q", green)
	}
	red := FormatVerdict(Verdict{SHA: "abcdefabcdefabcdef", Verdict: "red", Failing: []string{"TestX", "TestY"}})
	if !strings.Contains(red, "RED @abcdefabcdef") || !strings.Contains(red, "2 failing") || !strings.Contains(red, "TestX") || !strings.Contains(red, "TestY") {
		t.Errorf("red format = %q", red)
	}
}
