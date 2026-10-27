package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A repo that HAS a profile whose origin cannot be read is not ungoverned: it is a "cannot confirm",
// which enforce refuses and advisory allows. It must never be reported as unconfigured, whatever
// else it carries. The unconfigured state is only asked about a verdict of "no profile governs",
// and a profiled repo with an unreadable origin never gets that verdict, so it never reaches it.
func TestCLI_ProfiledRepoWithUnreadableOriginIsNeverUnconfigured(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)

	for _, tc := range []struct {
		name    string
		profile string
		refused bool
	}{
		{"enforce", enforceProfile, true},
		{"advisory", advisoryProfile, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := mkOriginRepo(t, tc.profile, "github")
			mustWrite(t, filepath.Join(repo.dir, ".leadkit", "decisions.jsonl"), "{}\n")
			payload, err := json.Marshal(map[string]any{"cwd": repo.dir, "tool_name": "Bash", "tool_input": map[string]string{"command": "git push github main"}})
			if err != nil {
				t.Fatal(err)
			}

			out, code := runOriginCLI(t, bin, repo.dir, string(payload), nil, "hook", "push-gate")
			if code != 0 || strings.Contains(out, "UNCONFIGURED") || strings.Contains(out, unconfiguredMarker) {
				t.Fatalf("hook: a profiled repo is never unconfigured (exit %d):\n%s", code, out)
			}
			if tc.refused != strings.Contains(out, `"permissionDecision":"deny"`) {
				t.Errorf("hook: refused should be %v:\n%s", tc.refused, out)
			}

			stdin := "refs/heads/main " + repo.head + " refs/heads/main 0000000000000000000000000000000000000000\n"
			out, code = runOriginCLI(t, bin, repo.dir, stdin, nil, "gate", "check-push")
			if strings.Contains(out, "UNCONFIGURED") || (code != 0) != tc.refused {
				t.Errorf("pre-push check: refused should be %v, got exit %d:\n%s", tc.refused, code, out)
			}

			out, code = runOriginCLI(t, bin, repo.dir, "", nil, "push", "--repo", repo.dir, "--branch", "main", "--remote", "github", "--dry-run")
			// The governed verb refuses what it cannot confirm in every mode (its own, older rule).
			if strings.Contains(out, "UNCONFIGURED") || code == 0 {
				t.Errorf("leadkit push must refuse a profiled repo whose origin cannot be read, got exit %d:\n%s", code, out)
			}
		})
	}
}
