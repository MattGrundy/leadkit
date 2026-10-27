package main

// Ticket 204 through the real binary: push-gate's three states for "is this repo governed?".
// The hook fires on every push on the machine, so a repo with no sign of leadkit must stay
// allowed and silent; a repo whose profile exists and was turned away, or that carries a
// .leadkit/ directory with no profile found, must be REFUSED rather than read as exempt.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pushGateVerdictFor runs `leadkit hook push-gate` for a bare `git push` from repo, with the
// sandbox HOME home, and returns the deny reason ("" for an allow).
func pushGateVerdictFor(t *testing.T, bin, home, repo string, extraEnv ...string) (deny string) {
	t.Helper()
	return pushGateVerdictCmd(t, bin, home, repo, "git push", extraEnv...)
}

// pushGateVerdictCmd is pushGateVerdictFor for an arbitrary command run from dir, which need not
// be the repo's top level (the hook resolves the governing directory from it).
func pushGateVerdictCmd(t *testing.T, bin, home, dir, command string, extraEnv ...string) (deny string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"cwd": dir, "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "hook", "push-gate")
	cmd.Dir = dir
	cmd.Env = append(sandboxedEnv(home, extraEnv), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "REVIEW_GATE_BYPASS=")
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook push-gate: %v\n%s", err, out)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return ""
	}
	var res struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	if res.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("unexpected hook output: %s", out)
	}
	return res.HookSpecificOutput.PermissionDecisionReason
}

func TestCLI_PushGate_ThreeStatesOfGovernance(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	const ext = "https://github.com/acme/"

	cases := []struct {
		name     string
		setup    func(t *testing.T, home, repo string)
		origin   string
		wantDeny bool
		wantIn   []string // substrings of the refusal
		subdir   string   // run the push from this directory under the repo ("" = its top level)
		command  string   // the push to run ("" = a bare `git push`); %s is the repo's top level
		outside  bool     // run the hook from a directory outside the repo (the push names it with -C)
	}{
		{
			name:   "state 1: a repo with no sign of leadkit anywhere is allowed",
			origin: ext + "stranger.git",
			setup:  func(t *testing.T, home, repo string) {},
		},
		{
			name:   "state 1: a repo with a stray ordinary dir is still allowed",
			origin: ext + "stranger.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, "docs", "notes.md"), "x")
			},
		},
		{
			name:   "state 3: a profile was read and the repo is out of scope (no origin to gate)",
			origin: "",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: readable\n")
			},
		},
		{
			name:   "control: a readable profile still gates its own repo",
			origin: ext + "gated.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: gated\n")
			},
			wantDeny: true,
			wantIn:   []string{"no review record"},
		},
		{
			name:   "state 2: an in-repo profile that will not parse is refused",
			origin: ext + "badyaml.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: [unclosed\n  : : :\n")
			},
			wantDeny: true,
			wantIn:   []string{filepath.Join("leadkit.yaml"), "was not accepted", "fail-closed"},
		},
		{
			name:   "state 2: an external profile that will not parse is refused",
			origin: ext + "extbad.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "extbad.yaml"), "project_id: [unclosed\n")
			},
			wantDeny: true,
			wantIn:   []string{"extbad.yaml", "was not accepted"},
		},
		{
			name:   "state 2: an external profile whose project_origin does not match is refused",
			origin: "https://github.com/mallory/extorig.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "extorig.yaml"), "project_id: extorig\nproject_origin: acme/extorig\n")
			},
			wantDeny: true,
			wantIn:   []string{"extorig.yaml", "project_origin"},
		},
		{
			name:   "state 2: a .leadkit/ directory and no profile is refused, naming the path searched and HOME",
			origin: ext + "dotlk.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, ".leadkit", "owners.yaml"), "hard_block: []\n")
			},
			wantDeny: true,
			wantIn:   []string{"has a .leadkit directory", "no profile was found", "dotlk.yaml", "HOME="},
		},
		{
			// The commonest governed shape: leadkit's own default paths live under .leadkit/, so a
			// repo with a readable profile nearly always has one. Out of scope here (no origin), the
			// profile WAS read, so the .leadkit/ directory must not be read as "no profile found".
			name:   "state 3 with a .leadkit/ directory: a readable profile, out of scope, is allowed",
			origin: "",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, ".leadkit", "owners.yaml"), "hard_block: []\n")
				mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: readable\n")
			},
		},
		{
			// The evidence is read at the governing top level, not at the directory the push is typed
			// from: a push from a subdirectory must not fall back to "no sign of leadkit".
			name:   "state 2 from a subdirectory is still refused",
			origin: ext + "dotlksub.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, ".leadkit", "owners.yaml"), "hard_block: []\n")
				mustWrite(t, filepath.Join(repo, "sub", "deep", "f.txt"), "x")
			},
			subdir:   filepath.Join("sub", "deep"),
			wantDeny: true,
			wantIn:   []string{"has a .leadkit directory", "no profile was found"},
		},
		{
			name:   "state 2 through git -C into a subdirectory is still refused",
			origin: ext + "dotlkc.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, ".leadkit", "owners.yaml"), "hard_block: []\n")
				mustWrite(t, filepath.Join(repo, "sub", "f.txt"), "x")
			},
			command:  "git -C %s/sub push",
			outside:  true,
			wantDeny: true,
			wantIn:   []string{"has a .leadkit directory", "no profile was found"},
		},
		{
			name:   "state 2 is not state 1: a .leadkit/ directory with a readable profile is judged on its profile",
			origin: ext + "dotlkok.git",
			setup: func(t *testing.T, home, repo string) {
				mustWrite(t, filepath.Join(repo, ".leadkit", "owners.yaml"), "hard_block: []\n")
				mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: dotlkok\n")
			},
			wantDeny: true,
			wantIn:   []string{"no review record"}, // in scope, ungated: refused for its record, not its profile
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			repo := filepath.Join(t.TempDir(), strings.TrimSuffix(filepath.Base(tc.origin), ".git"))
			if tc.origin == "" {
				repo = filepath.Join(t.TempDir(), "noorigin")
			}
			initRepoWithOrigin(t, repo, tc.origin)
			tc.setup(t, home, repo)

			command := "git push"
			if tc.command != "" {
				command = strings.ReplaceAll(tc.command, "%s", repo)
			}
			from := filepath.Join(repo, tc.subdir)
			if tc.outside {
				from = t.TempDir()
			}
			got := pushGateVerdictCmd(t, bin, home, from, command)
			if tc.wantDeny != (got != "") {
				t.Fatalf("deny = %v, want %v; reason: %q", got != "", tc.wantDeny, got)
			}
			for _, sub := range tc.wantIn {
				if !strings.Contains(got, sub) {
					t.Errorf("refusal lacks %q:\n%s", sub, got)
				}
			}
			if tc.wantDeny && strings.Contains(got, "no review record") == strings.Contains(got, "cannot be known") {
				t.Errorf("the refusal must be for exactly one cause (record XOR unreadable profile):\n%s", got)
			}
		})
	}
}

// The override still works for the new refusal: it is a refusal of an unknown, not a policy.
func TestCLI_PushGate_UnreadableProfile_BypassEnvAllows(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	home := t.TempDir()
	repo := filepath.Join(t.TempDir(), "badyaml")
	initRepoWithOrigin(t, repo, "https://github.com/acme/badyaml.git")
	mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: [unclosed\n")
	if got := pushGateVerdictFor(t, bin, home, repo); got == "" {
		t.Fatal("fixture is not refused without the bypass")
	}

	payload := `{"cwd":"` + repo + `","tool_name":"Bash","tool_input":{"command":"git push"}}`
	cmd := exec.Command(bin, "hook", "push-gate")
	cmd.Dir = repo
	cmd.Env = append(sandboxedEnv(home, nil), "REVIEW_GATE_BYPASS=deliberate")
	cmd.Stdin = strings.NewReader(payload)
	if out, err := cmd.Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("want a silent allow under REVIEW_GATE_BYPASS, got %v %q", err, out)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
