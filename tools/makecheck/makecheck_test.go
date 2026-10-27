// Package makecheck pins the guards `make check` runs against ci.yml. They are shell in a Makefile, so
// the only way to know they still refuse is to run them against a ci.yml that has drifted.
package makecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// ciDrift runs `make ci-drift` against the given ci.yml text.
func ciDrift(t *testing.T, ciYML string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	f := filepath.Join(t.TempDir(), "ci.yml")
	if err := os.WriteFile(f, []byte(ciYML), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "--no-print-directory", "ci-drift", "CI_YML="+f)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func realCI(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCIDrift_TheRealCIPasses(t *testing.T) {
	if out, err := ciDrift(t, realCI(t)); err != nil {
		t.Fatalf("the checked-in ci.yml must satisfy the guard: %v\n%s", err, out)
	}
}

// A job ci.yml gains that `make check` has not classified is refused.
func TestCIDrift_AJobTheMakefileDoesNotKnowIsRefused(t *testing.T) {
	ci := strings.Replace(realCI(t), "\n  secret-scan:\n", "\n  docs-lint:\n    runs-on: ubuntu-latest\n    steps: []\n\n  secret-scan:\n", 1)
	if !strings.Contains(ci, "docs-lint") {
		t.Fatal("fixture did not apply")
	}
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "disagree about CI's jobs") || !strings.Contains(out, "docs-lint") {
		t.Fatalf("a new job must be refused and named: %v\n%s", err, out)
	}
}

// A step added to an existing job is refused: a job-id comparison alone would pass it.
func TestCIDrift_AStepAddedToBuildTestLintIsRefused(t *testing.T) {
	ci := strings.Replace(realCI(t), "      - name: go mod tidy is clean\n", "      - name: make docs\n        run: make docs\n\n      - name: go mod tidy is clean\n", 1)
	if !strings.Contains(ci, "make docs") {
		t.Fatal("fixture did not apply")
	}
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "build-test-lint steps differ") || !strings.Contains(out, "make docs") {
		t.Fatalf("a new step in build-test-lint must be refused and named: %v\n%s", err, out)
	}
}

func TestCIDrift_AStepRemovedFromBuildTestLintIsRefused(t *testing.T) {
	ci := strings.Replace(realCI(t), "      - name: go vet\n        run: go vet ./...\n", "", 1)
	if strings.Contains(ci, "name: go vet") {
		t.Fatal("fixture did not apply")
	}
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "build-test-lint steps differ") {
		t.Fatalf("a dropped step must be refused: %v\n%s", err, out)
	}
}

// A step added to another job is not build-test-lint's, so it must not trip the build-test-lint comparison.
func TestCIDrift_AStepInAnotherJobIsNotBuildTestLints(t *testing.T) {
	ci := strings.Replace(realCI(t), "      - name: shellcheck\n", "      - name: extra installer step\n        run: true\n\n      - name: shellcheck\n", 1)
	if !strings.Contains(ci, "extra installer step") {
		t.Fatal("fixture did not apply")
	}
	if out, err := ciDrift(t, ci); err != nil {
		t.Fatalf("a step in the installer job must not be refused here: %v\n%s", err, out)
	}
}

// A job header with a trailing comment is still a job.
func TestCIDrift_AJobWithATrailingCommentIsStillSeen(t *testing.T) {
	ci := strings.Replace(realCI(t), "\n  secret-scan:\n", "\n  docs-lint: # new\n    runs-on: ubuntu-latest\n    steps: []\n\n  secret-scan:\n", 1)
	if !strings.Contains(ci, "docs-lint") {
		t.Fatal("fixture did not apply")
	}
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "disagree about CI's jobs") || !strings.Contains(out, "docs-lint") {
		t.Fatalf("a commented job header must be refused and named: %v\n%s", err, out)
	}
}

func TestCIDrift_AJobCIDroppedIsRefused(t *testing.T) {
	ci := strings.Replace(realCI(t), "\n  installer:\n", "\n  installer-renamed:\n", 1)
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "disagree about CI's jobs") {
		t.Fatalf("a listed job that ci.yml no longer has must be refused: %v\n%s", err, out)
	}
}

func TestCIDrift_ALintPinThatMovedIsRefused(t *testing.T) {
	ci := strings.Replace(realCI(t), "version: v2.11.3", "version: v9.9.9", 1)
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "pins golangci-lint 'v9.9.9'") {
		t.Fatalf("a drifted lint pin must be refused: %v\n%s", err, out)
	}
}

// An empty job list would make the comparison below vacuous in one direction, so it is its own refusal.
func TestCIDrift_NoJobsAtAllIsRefusedNotPassed(t *testing.T) {
	out, err := ciDrift(t, "name: CI\n        version: v2.11.3\n        go-version-file: go.mod\n")
	if err == nil || !strings.Contains(out, "found no jobs") {
		t.Fatalf("a ci.yml with no jobs must be refused: %v\n%s", err, out)
	}
}

// `check` runs the drift guard first and ci.yml's steps in ci.yml's order; this reads the plan
// `make -n check` prints, so nothing is executed.
func TestCheck_RunsTheDriftGuardThenCIsStepsInOrder(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	cmd := exec.Command("make", "-n", "check")
	cmd.Dir = repoRoot(t)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n check: %v\n%s", err, b)
	}
	plan := string(b)
	order := []string{"disagree about CI's jobs", "go build ./...", "go vet ./...", "gofmt -l .", "go test ./... -count=1", "make cover", "golangci-lint/v2/cmd/golangci-lint@", "go mod tidy -diff"}
	prev := -1
	for _, step := range order {
		at := strings.Index(plan, step)
		if at < 0 {
			t.Fatalf("`make check` does not run %q:\n%s", step, plan)
		}
		if at < prev {
			t.Errorf("%q runs out of order (ci.yml's order is build, vet, gofmt, test, cover, lint, tidy)", step)
		}
		prev = at
	}
}

func TestCIDrift_AToolchainThatMovedOffGoModIsRefused(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	cmd := exec.Command("make", "--no-print-directory", "ci-drift", "CHECK_TOOLCHAIN=go1.99.0")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "CHECK_TOOLCHAIN") {
		t.Fatalf("a toolchain pin that is not go.mod's go version must be refused: %v\n%s", err, out)
	}
}

func TestCIDrift_CIPinningItsOwnGoVersionIsRefused(t *testing.T) {
	ci := strings.Replace(realCI(t), "go-version-file: go.mod", "go-version: '1.26'", 1)
	out, err := ciDrift(t, ci)
	if err == nil || !strings.Contains(out, "go.mod") {
		t.Fatalf("a ci.yml that stops reading go.mod's version must be refused: %v\n%s", err, out)
	}
}
