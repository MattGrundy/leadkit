package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_TicketLink is ticket 265. `pr create --task T` records the PR's URL on the ticket, which is
// the link; `pr merge` then finds the ticket as the one recording that URL (no flag to remember),
// puts the `Leadkit-Merge` marker and `Ticket: T` in the commit it asks the forge to create, and
// records the sha the forge says that became. It never changes the ticket's status: merged is a
// fact, complete is a decision.
//
// The fake gh is the forge: it logs what it was asked for and answers `pr view --json
// state,mergeCommit` with a sha of its own choosing, so "the sha it actually merged" is a value the
// test controls and can tell apart from the PR head and from the sha the ticket started with.
func TestCLI_TicketLink(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := t.TempDir()
	gitEnv := append(sandboxedEnv(home, nil), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = repo, gitEnv
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	bare := filepath.Join(t.TempDir(), "forge.git")
	git("init", "-q", "--bare", bare)
	git("remote", "add", "origin", bare)
	commit("root")
	git("branch", "-M", "main")
	git("push", "-q", "origin", "main")
	git("fetch", "-q", "origin")
	git("checkout", "-q", "-b", "feature")
	head := commit("the only commit beyond main")
	git("push", "-q", "origin", "feature")
	git("fetch", "-q", "origin")

	const merged = "9999999999999999999999999999999999999999" // what the forge says the merge became
	const oldSHA = "1111111111111111111111111111111111111111" // what the ticket said before
	stubs := t.TempDir()
	mergeLog := filepath.Join(stubs, "merges.log")
	notYet := filepath.Join(stubs, "not-merged-yet")
	createLog := filepath.Join(stubs, "creates.log")
	noURL := filepath.Join(stubs, "no-url")
	const prURL = "https://github.com/forge/repo/pull/9"
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  \"pr view\") case \"$*\" in\n" +
		"    *mergeCommit*) if [ -e " + notYet + " ]; then echo '{\"state\":\"OPEN\",\"mergeCommit\":null}'; else echo '{\"state\":\"MERGED\",\"mergeCommit\":{\"oid\":\"" + merged + "\"}}'; fi ;;\n" +
		"    *commits*) echo '{\"title\":\"t\",\"number\":9,\"commits\":[{\"messageHeadline\":\"h\",\"messageBody\":\"\"}]}' ;;\n" +
		"    *) echo '{\"state\":\"OPEN\",\"isDraft\":false,\"mergeable\":\"MERGEABLE\",\"mergeStateStatus\":\"CLEAN\",\"headRefOid\":\"" + head + "\",\"headRefName\":\"feature\",\"title\":\"t\",\"baseRefName\":\"main\",\"url\":\"" + prURL + "\"}' ;; esac; exit 0 ;;\n" +
		"  \"pr checks\") exit 0 ;;\n" +
		"  \"pr create\") echo \"$@\" >> " + createLog + "; if [ ! -e " + noURL + " ]; then echo 'Warning: see https://github.com/forge/repo/pull/1'; echo " + prURL + "; fi; exit 0 ;;\n" +
		"  \"pr merge\") echo \"$@\" >> " + mergeLog + "; exit 0 ;;\n" +
		"  api\\ repos/*/commits/" + merged + ") printf '%s\\n' '{\"commit\":{\"message\":\"t (#9)\\n\\n* h\\n\\nLeadkit-Merge: pr-merge\\nTicket: 265\"}}'; exit 0 ;;\n" +
		"  api\\ repos/*/commits/*) echo '{\"commit\":{\"message\":\"plain\"}}'; exit 0 ;;\nesac\nexit 99\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(repo, "merge.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n    can_merge: true\n    merge_grant: test grant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(repo, "tasks", "265-the-ticket.md")
	second := filepath.Join(repo, "tasks", "266-other.md")
	bodyFile := filepath.Join(repo, "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const ticketText = "---\nid: 265\ntitle: The ticket\nstatus: gated\ntype: developer\nsha: " + oldSHA + "\nstage: finalise\ntrackers: {}\n---\n\nthe body\n"
	resetTicket := func() {
		t.Helper()
		_ = os.Remove(mergeLog)
		_ = os.Remove(createLog)
		_ = os.Remove(notYet)
		_ = os.Remove(noURL)
		_ = os.Remove(second)
		if err := os.WriteFile(ticketPath, []byte(ticketText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runLK := func(args ...string) (string, int) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir, c.Env = repo, sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile, "PATH=" + stubs + ":" + os.Getenv("PATH")})
		out, err := c.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	for _, layer := range [][2]string{{"code-review", "cr-1"}, {"critique", "crit-1"}} {
		if out, code := runLK("gate", "record", "--repo", repo, "--sha", head, "--layer", layer[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", layer[1]); code != 0 {
			t.Fatalf("gate record %s: exit %d\n%s", layer[0], code, out)
		}
	}
	create := func(extra ...string) (string, int) {
		t.Helper()
		return runLK(append([]string{"pr", "create", "--base", "main", "--head", "feature", "--title", "t", "--body-file", bodyFile, "--repo", repo}, extra...)...)
	}
	merge := func(extra ...string) (string, int) {
		t.Helper()
		return runLK(append([]string{"pr", "merge", "--pr", "9", "--repo", repo}, extra...)...)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	field := func(key string) string {
		for _, l := range strings.Split(read(ticketPath), "\n") {
			if v, ok := strings.CutPrefix(l, key+": "); ok {
				return v
			}
		}
		return ""
	}

	const withPR = "trackers: {pr: " + prURL + "}"

	t.Run("pr create --task records the PR's URL on the ticket and changes nothing else", func(t *testing.T) {
		resetTicket()
		out, code := create("--task", "265")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if got := read(ticketPath); got != strings.Replace(ticketText, "trackers: {}", withPR, 1) {
			t.Errorf("the only change must be trackers.pr = the URL the forge printed (the LAST one, not the warning's):\n%s", got)
		}
		if !strings.Contains(out, "ticket 265: trackers.pr = "+prURL+" (status unchanged: gated)") {
			t.Errorf("the output must say what was recorded:\n%s", out)
		}
	})

	t.Run("pr create with no --task leaves the ticket alone and says so", func(t *testing.T) {
		resetTicket()
		out, code := create()
		if code != 0 || read(ticketPath) != ticketText || read(createLog) == "" || !strings.Contains(out, "no ticket records this PR") {
			t.Fatalf("exit %d, ticket:\n%s\n%s", code, read(ticketPath), out)
		}
	})

	t.Run("pr create refuses an unfindable ticket before the PR is opened", func(t *testing.T) {
		resetTicket()
		out, code := create("--task", "404")
		if code != 1 || read(createLog) != "" || !strings.Contains(out, "no PR was opened") {
			t.Fatalf("exit %d, forge saw %q:\n%s", code, read(createLog), out)
		}
	})

	t.Run("pr create whose output holds no URL exits 3 and says the PR was opened", func(t *testing.T) {
		resetTicket()
		if err := os.WriteFile(noURL, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := create("--task", "265")
		if code != 3 || read(createLog) == "" || read(ticketPath) != ticketText || !strings.Contains(out, "PR WAS OPENED but ticket 265 was NOT updated") {
			t.Fatalf("exit %d, ticket:\n%s\n%s", code, read(ticketPath), out)
		}
	})

	t.Run("pr create --task records a second PR over the first and says it replaced it", func(t *testing.T) {
		resetTicket()
		old := "https://github.com/forge/repo/pull/3"
		if err := os.WriteFile(ticketPath, []byte(strings.Replace(ticketText, "trackers: {}", "trackers: {pr: "+old+"}", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := create("--task", "265")
		if code != 0 || !strings.Contains(read(ticketPath), withPR) || !strings.Contains(out, old+" -> "+prURL) {
			t.Fatalf("exit %d, ticket:\n%s\n%s", code, read(ticketPath), out)
		}
	})

	t.Run("pr merge finds the ticket that recorded the PR, with no flag, and records the merge without closing it", func(t *testing.T) {
		resetTicket()
		if out, code := create("--task", "265"); code != 0 {
			t.Fatalf("create: exit %d:\n%s", code, out)
		}
		out, code := merge()
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if log := read(mergeLog); !strings.Contains(log, "--squash") || !strings.Contains(log, "Leadkit-Merge: pr-merge") || !strings.Contains(log, "Ticket: 265") {
			t.Fatalf("the forge must be asked for a squash whose message carries the marker and the ticket, saw %q", log)
		}
		if strings.Contains(out, "warning") {
			t.Errorf("a merge commit carrying both lines must not draw a warning:\n%s", out)
		}
		if got := field("status"); got != "gated" {
			t.Errorf("status = %q: a merge records a fact and must not close the ticket:\n%s", got, read(ticketPath))
		}
		if got := field("sha"); got != merged {
			t.Errorf("sha = %q, want the sha the forge merged (%s), not the old one or the PR head", got, merged)
		}
		if got := field("reviewed_sha"); got != head {
			t.Errorf("reviewed_sha = %q, want the head the gate judged (%s)", got, head)
		}
		if got := field("trackers"); got != "{pr: "+prURL+"}" || field("stage") != "finalise" || !strings.HasSuffix(read(ticketPath), "\nthe body\n") {
			t.Errorf("the rest of the ticket must be untouched:\n%s", read(ticketPath))
		}
		if !strings.Contains(out, "ticket 265: sha "+oldSHA+" -> "+merged) || !strings.Contains(out, "status unchanged (gated)") {
			t.Errorf("the output must say what changed:\n%s", out)
		}
	})

	t.Run("pr merge --task is an override for a ticket that never recorded the PR, and records it", func(t *testing.T) {
		resetTicket()
		out, code := merge("--task", "265")
		if code != 0 || !strings.Contains(read(mergeLog), "Ticket: 265") || field("sha") != merged || field("status") != "gated" || field("trackers") != "{pr: "+prURL+"}" {
			t.Fatalf("exit %d, gh saw %q, ticket:\n%s\n%s", code, read(mergeLog), read(ticketPath), out)
		}
	})

	t.Run("a merge no ticket records is still marked as a leadkit merge, links nothing and says so", func(t *testing.T) {
		resetTicket()
		out, code := merge()
		// The marker is what tells a merge made by this verb from one made by hand (ticket 289), so it is
		// written whether or not a ticket is known; only the Ticket line depends on one.
		if log := read(mergeLog); !strings.Contains(log, "Leadkit-Merge: pr-merge") || strings.Contains(log, "Ticket:") {
			t.Fatalf("an unticketed merge must carry the marker and no Ticket line, saw %q", log)
		}
		if code != 0 || read(ticketPath) != ticketText || !strings.Contains(out, "linked to no ticket") {
			t.Fatalf("exit %d, ticket:\n%s\n%s", code, read(ticketPath), out)
		}
	})

	t.Run("two tickets recording one PR refuse the merge rather than link the wrong one", func(t *testing.T) {
		resetTicket()
		both := strings.Replace(ticketText, "trackers: {}", withPR, 1)
		if err := os.WriteFile(ticketPath, []byte(both), 0o644); err != nil {
			t.Fatal(err)
		}
		other := strings.Replace(strings.Replace(both, "id: 265", "id: 266", 1), "title: The ticket", "title: Other", 1)
		if err := os.WriteFile(second, []byte(other), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := merge()
		if code != 1 || read(mergeLog) != "" || read(ticketPath) != both || !strings.Contains(out, "265, 266") || !strings.Contains(out, "--task") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
		// ...and the override names the one that is meant.
		if out, code := merge("--task", "266"); code != 0 || !strings.Contains(read(mergeLog), "Ticket: 266") {
			t.Fatalf("--task must break the tie: exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a claimant that cannot be written still counts: one tidy ticket and one malformed refuse the merge", func(t *testing.T) {
		resetTicket()
		tidy := strings.Replace(ticketText, "trackers: {}", withPR, 1)
		if err := os.WriteFile(ticketPath, []byte(tidy), 0o644); err != nil {
			t.Fatal(err)
		}
		// the id disagrees with the file name, so the ticket cannot be resolved for writing
		malformed := strings.Replace(strings.Replace(tidy, "id: 265", "id: 999", 1), "title: The ticket", "title: Other", 1)
		if err := os.WriteFile(second, []byte(malformed), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := merge()
		if code != 1 || read(mergeLog) != "" || read(ticketPath) != tidy || !strings.Contains(out, "265, 266") {
			t.Fatalf("a malformed second claimant must not turn two claimants into a sole one: exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a sole claimant that cannot be written refuses the merge, not merges it unlinked", func(t *testing.T) {
		resetTicket()
		malformed := strings.Replace(strings.Replace(ticketText, "trackers: {}", withPR, 1), "id: 265", "id: 999", 1)
		if err := os.WriteFile(ticketPath, []byte(malformed), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := merge()
		if code != 1 || read(mergeLog) != "" || !strings.Contains(out, "records this PR but") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a store the lookup cannot read refuses the merge rather than reading as no claimant", func(t *testing.T) {
		resetTicket()
		if err := os.WriteFile(second, []byte(strings.Replace(ticketText, "trackers: {}", withPR, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(second, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(second, 0o644)
		if f, err := os.Open(second); err == nil {
			f.Close()
			t.Skip("file permissions are not enforced here (running as root?)")
		}
		out, code := merge()
		if code != 1 || read(mergeLog) != "" || !strings.Contains(out, "could not look for the ticket") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("an override naming a different ticket than the one recording the PR warns that the next merge will refuse", func(t *testing.T) {
		resetTicket()
		claimant := strings.Replace(ticketText, "trackers: {}", withPR, 1)
		if err := os.WriteFile(ticketPath, []byte(claimant), 0o644); err != nil {
			t.Fatal(err)
		}
		other := strings.Replace(strings.Replace(ticketText, "id: 265", "id: 266", 1), "title: The ticket", "title: Other", 1)
		if err := os.WriteFile(second, []byte(other), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := merge("--task", "266")
		if code != 0 || !strings.Contains(out, "warning: ticket 265 already records this PR") || !strings.Contains(read(mergeLog), "Ticket: 266") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a ticket that cannot be found refuses before the forge is reached", func(t *testing.T) {
		resetTicket()
		out, code := merge("--task", "404")
		if code != 1 || read(mergeLog) != "" || !strings.Contains(out, "nothing was merged") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a rebase cannot carry the trailer, so a ticket-linked one is refused, not merged unlinked", func(t *testing.T) {
		resetTicket()
		out, code := merge("--task", "265", "--method", "rebase")
		if code != 1 || read(mergeLog) != "" || read(ticketPath) != ticketText || !strings.Contains(out, "REFUSED") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a dry run merges nothing and writes nothing but shows the link and where it came from", func(t *testing.T) {
		resetTicket()
		if out, code := create("--task", "265"); code != 0 {
			t.Fatalf("create: exit %d:\n%s", code, out)
		}
		before := read(ticketPath)
		out, code := merge("--dry-run")
		if code != 0 || read(mergeLog) != "" || read(ticketPath) != before || !strings.Contains(out, "ticket:       265, from trackers.pr") || !strings.Contains(out, "Ticket: 265") {
			t.Fatalf("exit %d, gh saw %q:\n%s", code, read(mergeLog), out)
		}
	})

	t.Run("a merge the forge has not completed exits 3 and says the PR is merged, ticket untouched", func(t *testing.T) {
		resetTicket()
		if err := os.WriteFile(notYet, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := merge("--task", "265")
		if code != 3 || read(mergeLog) == "" || read(ticketPath) != ticketText || !strings.Contains(out, "IS MERGED but ticket 265 was NOT updated") {
			t.Fatalf("exit %d, ticket:\n%s\n%s", code, read(ticketPath), out)
		}
	})
}
