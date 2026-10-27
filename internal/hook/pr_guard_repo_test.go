package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// Ticket 198. Creating, copying, renaming, deleting, archiving or changing the visibility of a
// repository is owner-direct and nothing an agent can pass lifts it. Every form is refused with a message
// naming what was attempted, and an ordinary read is not.

// repoGrantProfile is the strongest grant an agent could hold on the repository it is standing in: if the
// refusal can be lifted by authority, this lifts it.
func repoGrantProfile() *config.Profile {
	return &config.Profile{Authority: map[string]config.AuthoritySpec{
		"acme/widgets": {CanOpenPR: config.BoolOrHuman{Allow: true}, CanMerge: config.BoolOrHuman{Allow: true}, MergeGrant: "INC-1", RawPRVerbs: []string{"create", "edit", "merge"}},
	}}
}

var forgeRepoRefused = []string{
	// the verbs
	"gh repo create throwaway --private", "gh repo create acme/throwaway --public --source=. --push", "gh repo fork acme/widgets --clone",
	"gh repo rename new-name", "gh repo delete acme/throwaway --yes", "gh repo archive acme/widgets --yes", "gh repo unarchive acme/widgets",
	"gh repo edit --visibility public --accept-visibility-change-consequences", "gh repo edit acme/widgets --visibility private",
	"glab repo create throwaway", "glab repo fork", "glab repo delete acme/throwaway", "glab repo archive", "glab repo transfer",
	"glab project create throwaway", "glab repo update --visibility public",
	// flags between the CLI and the noun
	"gh -R acme/widgets repo delete --yes",
	// joined, wrapped, or run from text
	"true && gh repo create x", "gh repo view; gh repo create x", "(gh repo create x)", "{ gh repo delete x --yes; }", "! gh repo create x",
	"echo ok | gh repo create x", "FOO=1 gh repo create x", `bash -c "gh repo create x"`, `eval 'gh repo delete x --yes'`,
	"xargs gh repo delete --yes <<< acme/x", `python3 <<< "gh repo delete x --yes"`, `ruby <<< "gh api -X DELETE repos/o/r"`, "gh api --field=private=false repos/acme/widgets", "echo $(gh repo create x)", "cd /tmp && gh repo fork acme/widgets",
	// an interpreter's inline program names the call (the incident's route, in the one form a line can show)
	`python3 -c "import os; os.system('gh repo create x --private')"`,
	`python3 -c "import subprocess; subprocess.run(['gh','repo','create','x'])"`,
	`python3.11 -c "import subprocess; subprocess.run(['glab','repo','create','x'], check=True)"`,
	`python3 -c "import subprocess; subprocess.run(['gh','api','-X','POST','user/repos','-f','name=x'])"`,
	`node -e "require('child_process').execSync('gh repo delete x --yes')"`,
	`node -e "require('child_process').execFileSync('gh', ['repo', 'fork', 'o/r'])"`,
	`ruby -e 'system("gh", "repo", "rename", "y")'`, `perl -e 'system("gh repo archive o/r")'`, `php -r 'exec("glab project create x");'`,
	`env python3 -c "import os; os.system('gh repo create x')"`, `nohup node -e "require('child_process').exec('gh repo create x')"`,
	`python3 <<< "import subprocess; subprocess.run(['gh','repo','create','x'])"`,
	`node -e 'require("child_process").exec("gh \"repo\" create x")'`,
	"python3 - <<'EOF'\nimport subprocess\nsubprocess.run(['gh', 'repo', 'create', 'x'])\nEOF",
	"cat <<'EOF' | python3\nimport os\nos.system(\"gh api -X DELETE repos/o/r\")\nEOF",
	// a program that only MENTIONS the call is refused too: the reading is of text, and it errs the safe way
	`ruby -e 'puts "gh repo create is owner-direct"'`,
	// a program PIPED to an interpreter, and the interpreters and launchers a first list left out
	`echo "import os; os.system('gh repo create x')" | python3`, `echo "require('child_process').execSync('gh repo create x')" | node`,
	`printf '%s' 'system("gh repo delete x --yes")' | ruby`, `echo 'gh repo create x' | python3 -`,
	`uv run python -c "import os; os.system('gh repo create x')"`, `npx -y tsx -e "require('child_process').execSync('gh repo create x')"`,
	`poetry run python -c "import subprocess; subprocess.run(['gh','repo','create','x'])"`,
	`awk 'BEGIN{system("gh repo create x")}'`, `Rscript -e 'system("gh repo create x")'`, `julia -e 'run(` + "`gh repo create x`" + `)'`,
	`pwsh -c "gh repo create x"`, `tclsh <<< "exec gh repo create x"`, `busybox sh -c "gh repo create x"`,
	// repository-level acts the first endpoint list left out
	"gh api -X POST repos/o/tmpl/generate -f name=x", "gh api repos/o/tmpl/generate -f owner=o -f name=x",
	"glab repo update my-project --archive", "gh repo edit o/r --archived", "glab api -X POST projects/1/fork/2", "glab api projects/user/5 -f name=x",
	"glab api -X POST import/github -f personal_access_token=x", "glab api -X POST projects/1/archive", "glab api -X PUT groups/g/projects/5",
	"glab api -X POST projects/remote-import -f url=u",
	`gh api graphql -f query='mutation{createRepository(input:{name:"x",visibility:PRIVATE}){repository{id}}}'`,
	`gh api graphql -f query='mutation { archiveRepository(input:{repositoryId:"R"}) { clientMutationId } }'`,
	`gh api graphql --raw-field query='mutation{cloneTemplateRepository(input:{}){clientMutationId}}'`,
	// a program piped to an interpreter from anywhere in the line: a group, a subshell, a variable, a nested script
	`(echo "import os; os.system('gh repo create x')") | python3`, `{ echo "import os; os.system('gh repo create x')"; } | python3`,
	`bash -c "echo \"import os; os.system('gh repo create x')\" | python3"`,
	`P="import os; os.system('gh repo create x')"; echo "$P" | python3`, `echo "import os; os.system('gh repo create x')" | tee /dev/null | python3 -`,
	`X=$(echo "import os; os.system('gh repo create x')") && echo "$X" | python3 -u`,
	`echo 'echo "import subprocess; subprocess.run([\"gh\",\"repo\",\"create\",\"x\"])" | python3' | sh`,
	`xargs -I{} sh -c {} <<< 'echo "import subprocess; subprocess.run([\"gh\",\"repo\",\"create\",\"x\"])" | python3'`,
	`bash <<< 'echo "import subprocess; subprocess.run([\"gh\",\"repo\",\"create\",\"x\"])" | python3'`,
	// the API forms
	"gh api -X POST /user/repos -f name=throwaway", "gh api user/repos -f name=throwaway", "gh api --method POST orgs/acme/repos -f name=x",
	"gh api -X POST repos/acme/widgets/forks", "gh api -X POST repos/acme/widgets/transfer -f new_owner=evil",
	"gh api -X DELETE repos/acme/throwaway", "gh api -X PATCH repos/acme/widgets -f private=false", "gh api --method=PATCH /repos/acme/widgets -F visibility=public",
	"gh api repos/acme/widgets -f name=renamed", `gh api -X POST "user/repos" --input body.json`, "glab api -X POST projects -f name=x",
	"glab api -X DELETE projects/42", "glab api -X POST projects/42/fork",
}

var forgeRepoAllowed = []string{
	"gh repo view", "gh repo view acme/widgets --json name", "gh repo list acme", "gh repo clone acme/widgets", "gh repo set-default acme/widgets",
	"gh repo sync", "gh repo edit --description 'a tool'", "gh repo edit --add-topic go", "glab repo view", "glab repo list", "glab repo clone acme/widgets",
	"gh api user/repos", "gh api -X GET user/repos", "gh api repos/acme/widgets", "gh api repos/acme/widgets/pulls", "gh api orgs/acme/repos --paginate",
	"gh api repos/acme/widgets/issues -f title=x", "gh api repos/acme/widgets/contents/x.md", "gh issue create --title x", "gh pr list", "gh pr view 5",
	`python3 -c "print('gh repo view')"`, `python3 -c "import subprocess; subprocess.run(['gh','repo','view','o/r'])"`,
	`node -e "require('child_process').execSync('gh api repos/o/r --jq .private')"`, `ruby -e 'puts "gh repo view"'`,
	`python3 -c "print(1)"`, "python3 tool.py", "node build.js", `perl -e 'print "hello"'`,
	`gh api graphql -f query='query{viewer{login}}'`, `gh api graphql -f query='mutation{addStar(input:{starrableId:"S"}){clientMutationId}}'`,
	"glab repo update my-project --description d", "gh api repos/o/r/generate", "glab api projects/1/forks", "glab api projects/1/repository/tree",
	`echo "print(1)" | python3`, `gh repo view | python3 -m json.tool`, `uv run pytest`, `awk '{print $1}' f.txt`, `busybox ls`,
	`(echo "print(1)") | python3`, `P="print(1)"; echo "$P" | python3`, `git commit -m "gh repo create is owner-direct" && python3 x.py`, `echo "gh repo create" | python3 -m json.tool`,
	"git clone https://github.com/acme/widgets", "echo repo create", "gh --version", "ls repo", "glab api projects", "glab api projects/42",
}

func TestPRGuard_RepositoryLevelActsAreRefusedForEveryoneAndEveryForm(t *testing.T) {
	for name, prof := range map[string]*config.Profile{"no profile": nil, "the strongest grant": repoGrantProfile()} {
		for _, cmd := range forgeRepoRefused {
			res := PRGuard(bashInput(cmd), prof, "")
			assertDeny(t, res, name+": "+cmd)
			for _, want := range []string{"owner-direct", "no flag, authority entry or sentinel", "outward-facing"} {
				if !strings.Contains(res.Stdout, want) {
					t.Errorf("%s / %q: the refusal must say %q, got:\n%s", name, cmd, want, res.Stdout)
				}
			}
		}
	}
	// It names the verb that was attempted.
	if res := PRGuard(bashInput("gh repo delete acme/x --yes"), nil, ""); !strings.Contains(res.Stdout, "gh repo delete") {
		t.Errorf("the refusal must name the verb: %s", res.Stdout)
	}
	if res := PRGuard(bashInput("gh api -X DELETE repos/acme/x"), nil, ""); !strings.Contains(res.Stdout, "gh api") {
		t.Errorf("the refusal must name the API form: %s", res.Stdout)
	}
}

func TestPRGuard_ReadingARepositoryIsNotRefused(t *testing.T) {
	for _, cmd := range forgeRepoAllowed {
		assertAllow(t, PRGuard(bashInput(cmd), repoGrantProfile(), ""), cmd)
	}
}

// The refusal is not a PR verb's, so it must hold whichever way pr-guard would otherwise have judged the
// line, and the PR refusals it sits beside are unchanged.
func TestPRGuard_TheRepoRefusalDoesNotDisturbThePRVerbs(t *testing.T) {
	assertDeny(t, PRGuard(bashInput("gh pr merge 5"), repoGrantProfile(), ""), "a raw merge is still refused")
	if res := PRGuard(bashInput("gh pr merge 5"), nil, ""); strings.Contains(res.Stdout, "owner-direct") {
		t.Errorf("a PR refusal must keep its own message: %s", res.Stdout)
	}
}
