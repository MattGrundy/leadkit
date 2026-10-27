package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trustHomeEnv makes the unconfigured push-gate state reachable from a sandbox HOME. It must be
// settable only by the test suite's own build: a binary that anyone runs for real must always ask
// the account. These pin the three ways that could stop being true.

// The variable carries no initial value in source, so a build with no -X flag has it empty.
func TestTrustHomeEnv_HasNoDefaultAndIsNotSetByTheProcess(t *testing.T) {
	if trustHomeEnv != "" {
		t.Fatalf("this test binary was built without the flag, so trustHomeEnv must be empty, got %q", trustHomeEnv)
	}
	src, err := os.ReadFile("account_home.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "\nvar trustHomeEnv string\n") {
		t.Error("trustHomeEnv must be declared `var trustHomeEnv string` with no initializer")
	}
	// An environment variable or a command line cannot flip it: only the linker can.
	t.Setenv("trustHomeEnv", "1")
	t.Setenv("LEADKIT_TRUST_HOME", "1")
	t.Setenv("HOME", t.TempDir())
	if homeIsTheAccountsOwn() {
		t.Error("a temp HOME must not be the account's, whatever the environment says")
	}
}

// Nothing that builds a release names it, and the only place the flag is passed is the test
// suite's own binary build.
func TestTrustHomeEnv_NoReleaseBuildSetsIt(t *testing.T) {
	root := repoRootFromCwd(t)
	var files []string
	for _, pat := range []string{"Makefile", ".github/workflows/*", "scripts/*", ".goreleaser*", "Dockerfile*"} {
		m, _ := filepath.Glob(filepath.Join(root, pat))
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("found no build files to check; the pattern list is stale")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "trustHomeEnv") {
			t.Errorf("%s sets main.trustHomeEnv: only the test suite's binary build may", f)
		}
	}
	mains, _ := filepath.Glob(filepath.Join(root, "cmd", "leadkit", "*.go"))
	for _, f := range mains {
		if strings.HasSuffix(f, "_test.go") || filepath.Base(f) == "account_home.go" {
			continue
		}
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), "trustHomeEnv") {
			t.Errorf("%s reads or writes trustHomeEnv: only account_home.go may", f)
		}
	}
}
