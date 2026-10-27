package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Ground truth is the real shell. A logging gh and glab are first on PATH; each candidate line is RUN, for
// real, in bash, and the argv the shell actually handed the CLI is read back. If that argv is a repository
// mutation the guard must have refused the line, however it was quoted, escaped, grouped or wrapped. The
// first cut matched text, and a quote, an attached -XDELETE, an absolute API URL or a group walked past it.

func shimDir(t *testing.T) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "calls.log")
	for _, cli := range []string{"gh", "glab"} {
		script := "#!/bin/sh\nprintf '" + cli + "\\n' >> '" + log + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + log + "'; done\nprintf '\\0\\n' >> '" + log + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, cli), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, log
}

// requireCI skips the tests that run a shell, because they run the lines for real. They run in CI only
// (GITHUB_ACTIONS=true): on a workstation the environment carries a real forge login, and this file's first
// cut shows what a line that escapes its shim does with it. The environment given to the child (shimEnv) holds
// no credential either way, so an escape fails against a dead host.
func requireCI(t *testing.T) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("runs shell lines for real: CI only (GITHUB_ACTIONS=true); the table tests carry the behaviour elsewhere")
	}
}

// shimEnv is the whole environment a line runs in: nothing inherited, so no real GH_TOKEN, GITLAB_TOKEN or forge
// config reaches it. The shim is first on PATH, and if a CLI other than the shim is ever reached (a login shell
// rewrites PATH) it points at a host that does not answer, with a dummy credential.
func shimEnv(shim, home string) []string {
	return []string{
		"PATH=" + shim + string(os.PathListSeparator) + "/usr/bin:/bin",
		"HOME=" + home, "GH_CONFIG_DIR=" + filepath.Join(home, "gh"), "GLAB_CONFIG_DIR=" + filepath.Join(home, "glab"),
		"GH_HOST=127.0.0.1:1", "GITLAB_HOST=127.0.0.1:1", "GH_TOKEN=dummy", "GITHUB_TOKEN=dummy", "GITLAB_TOKEN=dummy", "GLAB_TOKEN=dummy",
		"GH_PROMPT_DISABLED=1", "NO_COLOR=1",
	}
}

// loginShell matches any way of asking a shell to start as a login or interactive shell: `-l`, `-i`, a bundle
// of short flags that contains either (`-lc`, `-il`, `-cl`), `--login`, `--interactive`.
var loginShell = regexp.MustCompile(`(^|\s)(-[a-zA-Z]*[li][a-zA-Z]*|--login|--interactive)(\s|$)`)

// runShimmed runs line in bash with the logging CLIs first on PATH, in work (a directory that is not a git work
// tree: a line such as `git commit --allow-empty -m ...` must not commit into this repository). The probe that
// the shim wins runs in the SAME shell as the line (it is the first thing that shell does, and the line follows
// it), and a line that asks for a login or interactive shell anywhere is refused before it runs.
func runShimmed(t *testing.T, shim, work, line string) {
	t.Helper()
	requireCI(t)
	if loginShell.MatchString(line) {
		t.Fatalf("a login or interactive shell can rewrite PATH and reach the real CLI: %q", line)
	}
	probe := `[ "$(command -v gh)" = '` + filepath.Join(shim, "gh") + `' ] && [ "$(command -v glab)" = '` + filepath.Join(shim, "glab") + `' ] || { echo "the shim does not win in this shell" >&2; exit 97; }
`
	cmd := exec.Command("bash", "-c", probe+line)
	cmd.Env = shimEnv(shim, work)
	cmd.Dir = work
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 97 {
		t.Fatalf("the shim in %s does not win `command -v` in the shell that runs the line: refusing %q", shim, line)
	}
}

// shimMutation is an INDEPENDENT reading of the clean argv the shell handed the CLI: looser than the guard's, and
// written without it, because ground truth classified by the code under test proves nothing (a mutant that breaks
// the guard's reading of a spelling breaks the arbiter with it, and the test stays green).
func shimMutation(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	args := argv[1:]
	lower := make([]string, len(args))
	for i, a := range args {
		lower[i] = strings.ToLower(a)
	}
	has := func(words ...string) bool {
		for _, a := range lower {
			for _, w := range words {
				if a == w {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("repo", "project"):
		if has("create", "fork", "rename", "delete", "archive", "unarchive", "transfer") {
			return true
		}
		if has("edit", "update") {
			for _, a := range lower {
				if strings.HasPrefix(a, "--visibility") || strings.HasPrefix(a, "--accept-visibility") {
					return true
				}
			}
		}
	case has("api"):
		write, repoish := false, false
		for _, a := range lower {
			up := strings.ToUpper(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(a, "--method"), "-x"), "="))
			if up == "POST" || up == "PUT" || up == "PATCH" || up == "DELETE" {
				write = true
			}
			if strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "--field") || strings.HasPrefix(a, "--raw-field") || strings.HasPrefix(a, "--input") {
				write = true
			}
			if !strings.HasPrefix(a, "-") && (strings.Contains(a, "user/repos") || strings.Contains(a, "/repos") || strings.HasPrefix(a, "repos/") || strings.Contains(a, "projects")) &&
				!strings.Contains(a, "/issues") && !strings.Contains(a, "/pulls") && !strings.Contains(a, "/contents") {
				repoish = true
			}
		}
		return write && repoish
	}
	return false
}

// performed returns every mutation the shim saw.
func performed(t *testing.T, log string) []string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var out []string
	for _, call := range strings.Split(string(data), "\x00\n") {
		lines := strings.Split(strings.TrimRight(call, "\n"), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		if shimMutation(lines) {
			out = append(out, strings.Join(lines, " "))
		}
	}
	return out
}

func TestPRGuard_RealShellGroundTruth_NoRepositoryMutationIsAllowed(t *testing.T) {
	requireCI(t)
	dir, log := shimDir(t)
	work := t.TempDir()
	spellings := []string{
		// the plain forms
		"gh repo create x/y --private", "gh repo fork a/b", "gh repo rename n", "gh repo delete a/b --yes", "gh repo archive a/b --yes",
		"glab repo create x", "glab repo delete a/b", "glab project create x", "glab repo fork",
		// quoting, escaping and path-spelling the CLI, the noun and the verb
		`gh 'repo' delete x/y --yes`, `gh repo "delete" x/y --yes`, `gh repo de"lete" x/y --yes`, `gh "repo" 'create' x`,
		`\gh repo delete x/y --yes`, `'gh' repo create x`, `"gh" repo delete x`, `"` + dir + `/gh" repo delete x/y --yes`, dir + `/gh repo delete x/y`,
		"gh REPO DELETE x/y --yes", "gh -R a/b repo delete --yes", "gh --repo a/b repo rename n",
		// visibility
		"gh repo edit --visibility public --accept-visibility-change-consequences", "gh repo edit a/b --visibility=private",
		// the API: every spelling of the method, an absolute URL, fields that make gh send a POST
		"gh api -X POST /user/repos -f name=t", "gh api user/repos -f name=t", "gh api -XDELETE repos/o/r", "gh api -X delete repos/o/r",
		"gh api --method=DELETE repos/o/r", "gh api --method delete repos/o/r", "gh api -X=DELETE repos/o/r",
		"gh api https://api.github.com/repos/o/r -X DELETE", "gh api https://api.github.com/orgs/o/repos -f name=x",
		"gh api -X POST repos/o/r/forks", "gh api -X PATCH repos/o/r -f private=false", "gh api repos/o/r -fname=renamed", "gh api --field=private=false repos/o/r", "gh api --raw-field=visibility=public repos/o/r", "gh api --input=f.json repos/o/r", "gh api 'repos/o/r' -X DELETE", "gh api \"repos/o/r\" -X DELETE",
		"gh api -X POST orgs/o/repos --input /dev/null", `gh api -X POST "user/repos" -f name=t`,
		"glab api -X DELETE projects/42", "glab api -X POST projects -f name=x", "glab api -XPOST projects/42/fork",
		"glab api https://gitlab.example.com/api/v4/projects/42 -X DELETE",
	}
	wrappers := []string{
		"%s", "(%s)", "{ %s; }", "! %s", "true && %s", "false || %s", "echo hi; %s", "%s &", "echo x | %s", "if true; then %s; fi",
		"FOO=1 %s", "time %s", "env %s", "command %s", "nohup %s", "cd /tmp && %s", "x=$(%s)", "echo $(%s)", "echo `%s`",
		`bash -c "%s"`, `sh -c '%s'`, `eval "%s"`, "xargs %s <<< x", `bash <<< "%s"`, "while false; do %s; done",
		"( { %s; } )", "{ (%s); }", "echo '%s' | sh", "echo '%s' | bash", "echo '%s' | bash -s", "printf '%s\\n' | sh",
		// A script fed to a shell as a heredoc body: the guard must read the body, not only the text around it.
		"sh <<EOF\n%s\nEOF", "bash <<'EOF'\n%s\nEOF", "cat <<EOF | sh\n%s\nEOF", "bash -s <<EOF\ntrue\n%s\nEOF",
	}
	checked, mutated := 0, 0
	for _, sp := range spellings {
		for _, w := range wrappers {
			line := strings.Replace(w, "%s", sp, 1)
			if strings.Contains(w, "echo '") || strings.Contains(w, "printf '") {
				line = strings.Replace(w, "%s", strings.ReplaceAll(sp, "'", `'\''`), 1)
			}
			if strings.HasPrefix(w, `bash -c "`) || strings.HasPrefix(w, `bash <<<`) || strings.HasPrefix(w, "eval") {
				line = strings.Replace(w, "%s", strings.ReplaceAll(sp, `"`, `\"`), 1)
			}
			_ = os.Remove(log)
			runShimmed(t, dir, work, line)
			checked++
			muts := performed(t, log)
			if len(muts) == 0 {
				continue
			}
			mutated++
			for _, prof := range []bool{false, true} {
				var res Result
				if prof {
					res = PRGuard(bashInput(line), repoGrantProfile(), "")
				} else {
					res = PRGuard(bashInput(line), nil, "")
				}
				if res.Stdout == "" {
					t.Errorf("the shell performed %v and the guard allowed it (strong grant=%v): %s", muts, prof, line)
				}
			}
		}
	}
	if mutated < checked/2 {
		t.Fatalf("the harness went blind: only %d of %d lines performed a mutation under the real shell", mutated, checked)
	}
}

// Readers and mentions, the same wrappers: nothing is refused that mutates nothing.
func TestPRGuard_RealShellGroundTruth_NothingThatMutatesNothingIsRefused(t *testing.T) {
	requireCI(t)
	dir, log := shimDir(t)
	work := t.TempDir()
	spellings := []string{
		"gh repo view", "gh repo view a/b --json name", "gh repo list o", "gh repo clone a/b", "gh repo sync", "gh repo set-default a/b",
		"gh repo edit --description d", "gh repo edit --add-topic go", "gh api user/repos", "gh api -X GET user/repos", "gh api -X get user/repos", "gh api --method=get user/repos", "gh api repos/o/r",
		"gh api repos/o/r/pulls", "gh api repos/o/r/issues -f title=x", "gh api orgs/o/repos --paginate", "gh api repos/o/r --jq .private",
		"glab repo view", "glab api projects", "glab api projects/42", "glab api -X GET projects/42", "gh pr list", "gh issue create --title t",
		"git status", "echo gh repo create x", `git commit --allow-empty -m "gh repo delete x"`, `grep "gh repo create" /dev/null`,
	}
	for _, sp := range spellings {
		for _, w := range []string{"%s", "(%s)", "{ %s; }", "true && %s", `bash -c "%s"`, "x=$(%s)"} {
			line := strings.Replace(w, "%s", strings.ReplaceAll(sp, `"`, `\"`), 1)
			if w == "%s" || w == "(%s)" || w == "{ %s; }" || w == "true && %s" {
				line = strings.Replace(w, "%s", sp, 1)
			}
			_ = os.Remove(log)
			runShimmed(t, dir, work, line)
			if muts := performed(t, log); len(muts) != 0 {
				t.Fatalf("setup: %q performs %v; it belongs in the other table", line, muts)
			}
			if res := PRGuard(bashInput(line), repoGrantProfile(), ""); res.Stdout != "" {
				t.Errorf("a line that mutates nothing was refused: %s\n%s", line, res.Stdout)
			}
		}
	}
}

// These two run on a workstation (they execute nothing): the safeguards of the tests above are themselves tested.
func TestRealShellHarness_AnyLoginOrInteractiveShellIsRefusedBeforeItRuns(t *testing.T) {
	for _, line := range []string{"bash -lc 'x'", "bash -l -c x", "sh -il", "bash -cl 'x'", "bash --login -c x", "bash -c x --interactive", "zsh -i -c x", "echo hi; bash -ec 'x'; bash -li"} {
		if !loginShell.MatchString(line) {
			t.Errorf("%q starts a login or interactive shell and must be refused", line)
		}
	}
	for _, line := range []string{"bash -c x", "sh -c 'gh repo view'", "gh api repos/o/r -fname=renamed", "gh api -XDELETE repos/o/r --jq .private", "gh repo edit --add-topic go", "xargs gh repo delete --yes <<< x", "bash -s <<EOF"} {
		if loginShell.MatchString(line) {
			t.Errorf("%q is not a login shell and must not be refused", line)
		}
	}
}

func TestRealShellHarness_TheChildEnvironmentCarriesNoRealCredentialAndPointsAtADeadHost(t *testing.T) {
	t.Setenv("GH_TOKEN", "real-looking-token")
	t.Setenv("GITLAB_TOKEN", "real-looking-token")
	env := strings.Join(shimEnv("/shim", "/home"), "\n")
	if strings.Contains(env, "real-looking-token") {
		t.Errorf("the child inherited a credential:\n%s", env)
	}
	for _, want := range []string{"PATH=/shim:", "GH_HOST=127.0.0.1:1", "GITLAB_HOST=127.0.0.1:1", "GH_TOKEN=dummy", "GITLAB_TOKEN=dummy"} {
		if !strings.Contains(env, want) {
			t.Errorf("the child environment must contain %q:\n%s", want, env)
		}
	}
}

func TestRealShellHarness_TheShellTestsSkipUnlessCI(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	skipped := false
	t.Run("inner", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		requireCI(t)
	})
	if !skipped {
		t.Error("requireCI must skip when GITHUB_ACTIONS is not true: the shell tests run lines for real")
	}
}
