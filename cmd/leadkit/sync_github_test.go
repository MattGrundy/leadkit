package main

// `sync run --output github` shells out to gh. Every test here puts a shim `gh` first
// on PATH, so the real gh — and the real GitHub — is unreachable; the shim logs each
// call so a test can assert on what would have been sent.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFlatTicketFixture lays out the shape of the live store: tickets with no parent
// and no milestones.yaml, which the hierarchy-shaped sync model drops entirely.
func writeFlatTicketFixture(t *testing.T, root string) (profilePath, storeRoot string) {
	t.Helper()
	storeRoot = filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"1-first.md":  "---\nid: 1\ntitle: \"First ticket\"\nstatus: pending\ntype: developer\npriority: P1\ntrackers: {}\n---\n\nSECRET-BODY-ONE\n",
		"2-second.md": "---\nid: 2\ntitle: Second ticket\nstatus: done\ntype: qa-agent\npriority: P2\ntrackers: {}\n---\n\nSECRET-BODY-TWO\n",
	} {
		if err := os.WriteFile(filepath.Join(storeRoot, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	profilePath = filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nissues_root: issues\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath, storeRoot
}

// ghShim writes a fake `gh` that logs "<stdin-or-> | <args>" per call to the returned log
// path: a listing returns an empty repo, a POST returns issue number 41, a PATCH {}, and
// a read of any single issue is a 404 (so the repo has no issues the listing hides).
func ghShim(t *testing.T) (pathEnv, logPath string) { return ghShimVisibility(t, "true") }

// ghShimVisibility is ghShim for a repo the forge reports with the given `.private` answer: the
// visibility query (ticket 196) comes before any other call, and is the only one a public repo gets.
func ghShimVisibility(t *testing.T, private string) (pathEnv, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
in=""
case "$*" in *--input*) in="$(cat)";; esac
printf '%s | %s\n' "$*" "$in" >> "` + logPath + `"
case "$*" in
  *--jq*.private*) echo '` + private + `';;
  *POST*) echo '{"number":41}';;
  *PATCH*) echo '{}';;
  *--paginate*) echo '[[]]';;
  *) echo 'gh: Not Found (HTTP 404)' >&2; exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), logPath
}

func readGHLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestCLI_SyncPlanGitHub_SeesParentlessTickets(t *testing.T) {
	bin := testBinary(t)
	profilePath, _ := writeFlatTicketFixture(t, t.TempDir())
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath}, "sync", "plan", "--output", "github")
	if code != 0 || !strings.Contains(out, "First ticket") || !strings.Contains(out, "2 op(s): 2 create") {
		t.Errorf("want both parentless tickets planned as creates, got %d:\n%s", code, out)
	}
	if strings.Contains(out, "SECRET-BODY") || strings.Contains(out, `\"First`) {
		t.Errorf("a plan must carry no body and no raw quotes:\n%s", out)
	}
	// the same store through linear shows the seam's other shape: nothing but the root
	lin, _ := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath}, "sync", "plan", "--output", "linear")
	if strings.Contains(lin, "First ticket") {
		t.Errorf("fixture is no longer parentless — linear planned a ticket:\n%s", lin)
	}
}

func TestCLI_SyncRunGitHub_DefaultIsADryRun(t *testing.T) {
	bin := testBinary(t)
	profilePath, store := writeFlatTicketFixture(t, t.TempDir())
	pathEnv, logPath := ghShim(t)
	before := snapshotFiles(t, store)

	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath, pathEnv}, "sync", "run", "--output", "github", "--repo", "acme/mirror")
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "would create") || !strings.Contains(out, "title + metadata only") {
		t.Errorf("want a metadata-only dry run, got %d:\n%s", code, out)
	}
	log := readGHLog(t, logPath)
	if strings.Contains(log, "POST") || strings.Contains(log, "PATCH") {
		t.Errorf("a dry run made a write call:\n%s", log)
	}
	after := snapshotFiles(t, store)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("dry run changed %s", k)
		}
	}
}

func TestCLI_SyncRunGitHub_ApplyCreatesRecordsAndSendsNoBodyByDefault(t *testing.T) {
	bin := testBinary(t)
	profilePath, store := writeFlatTicketFixture(t, t.TempDir())
	pathEnv, logPath := ghShim(t)
	env := []string{"LEADKIT_PROFILE=" + profilePath, pathEnv}

	out, code := runCLI(t, bin, env, "sync", "run", "--output", "github", "--repo", "acme/mirror", "--apply")
	if code != 0 || !strings.Contains(out, "2 created") {
		t.Fatalf("apply: %d\n%s", code, out)
	}
	if strings.Contains(readGHLog(t, logPath), "SECRET-BODY") {
		t.Error("a ticket body was sent without --with-bodies")
	}
	one, _ := os.ReadFile(filepath.Join(store, "1-first.md"))
	if !strings.Contains(string(one), "trackers: {github: acme/mirror#41}") {
		t.Errorf("id not recorded into the ticket:\n%s", one)
	}

	// --with-bodies is the deliberate act
	profile2, _ := writeFlatTicketFixture(t, t.TempDir())
	pathEnv2, log2 := ghShim(t)
	out, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profile2, pathEnv2}, "sync", "run", "--output", "github", "--repo", "acme/mirror", "--apply", "--with-bodies")
	if code != 0 || !strings.Contains(out, "BODIES INCLUDED") || !strings.Contains(readGHLog(t, log2), "SECRET-BODY-ONE") {
		t.Errorf("--with-bodies must publish and say so, got %d:\n%s", code, out)
	}
}

func TestCLI_SyncRunGitHub_UsageErrors(t *testing.T) {
	bin := testBinary(t)
	profilePath, _ := writeFlatTicketFixture(t, t.TempDir())
	pathEnv, logPath := ghShim(t)
	env := []string{"LEADKIT_PROFILE=" + profilePath, pathEnv}
	for name, args := range map[string][]string{
		"no repo":               {"sync", "run", "--output", "github"},
		"malformed repo":        {"sync", "run", "--output", "github", "--repo", "not a repo"},
		"mode is linear's":      {"sync", "run", "--output", "github", "--repo", "a/b", "--mode", "direct"},
		"github flag on linear": {"sync", "run", "--output", "linear", "--repo", "a/b"},
		"apply on linear":       {"sync", "run", "--output", "linear", "--apply"},
	} {
		if out, code := runCLI(t, bin, env, args...); code != 2 {
			t.Errorf("%s: want usage exit 2, got %d:\n%s", name, code, out)
		}
	}
	if log := readGHLog(t, logPath); log != "" {
		t.Errorf("a usage error reached gh:\n%s", log)
	}
}

// The count, end to end through the binary: N parentless tickets in the store means
// N tickets planned and N "would create" lines in the dry run. A sync that silently
// moves fewer is the defect ticket 193 described, and the one that would precede the
// backlog file being deleted (217). The dry run is used because --apply paces one
// second per write; the apply-side count is pinned in internal/sync.
func TestCLI_SyncRunGitHub_DryRunCountsEveryParentlessTicket(t *testing.T) {
	const n = 216
	bin := testBinary(t)
	root := t.TempDir()
	profilePath, store := writeFlatTicketFixture(t, root)
	for i := 3; i <= n; i++ { // the fixture holds tickets 1 and 2
		body := fmt.Sprintf("---\nid: %d\ntitle: ticket %d\nstatus: pending\ntype: developer\npriority: P2\ntrackers: {}\n---\n\nbody\n", i, i)
		if err := os.WriteFile(filepath.Join(store, fmt.Sprintf("%d-t.md", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pathEnv, logPath := ghShim(t)
	env := []string{"LEADKIT_PROFILE=" + profilePath, pathEnv}

	out, code := runCLI(t, bin, env, "sync", "run", "--output", "github", "--repo", "acme/mirror")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("%d ticket(s)", n)) || !strings.Contains(out, fmt.Sprintf("%d to create", n)) {
		t.Errorf("want %d tickets and %d to create, got %d:\n%s", n, n, code, tailOf(out, 400))
	}
	if got := strings.Count(out, "would create"); got != n {
		t.Errorf("%d \"would create\" lines, want %d", got, n)
	}
	if log := readGHLog(t, logPath); strings.Contains(log, "POST") || strings.Contains(log, "PATCH") {
		t.Errorf("a dry run made a write call")
	}

	plan, code := runCLI(t, bin, env, "sync", "plan", "--output", "github")
	if want := fmt.Sprintf("%d op(s): %d create", n, n); code != 0 || !strings.Contains(plan, want) {
		t.Errorf("sync plan: want %q, got %d:\n%s", want, code, tailOf(plan, 300))
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// Ticket 196 through the binary: a repo the forge does not report as private is refused for a dry run and an
// apply alike, and the forge sees the visibility question and nothing else.
func TestCLI_SyncRunGitHub_APublicRepoIsRefusedAndNothingElseIsCalled(t *testing.T) {
	bin := testBinary(t)
	for _, private := range []string{"false", "", "null"} {
		profile, _ := writeFlatTicketFixture(t, t.TempDir())
		pathEnv, log := ghShimVisibility(t, private)
		for _, apply := range []bool{false, true} {
			args := []string{"sync", "run", "--output", "github", "--repo", "acme/mirror"}
			if apply {
				args = append(args, "--apply")
			}
			out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profile, pathEnv}, args...)
			if code != 1 || !strings.Contains(out, "refusing acme/mirror") || !strings.Contains(out, "published title cannot be unpublished") {
				t.Errorf("private=%q apply=%v: a repo not confirmed private must be refused, exit %d:\n%s", private, apply, code, out)
			}
		}
		calls := readGHLog(t, log)
		if n := strings.Count(calls, "\n"); n != 2 || strings.Contains(calls, "issues") || strings.Contains(calls, "POST") {
			t.Errorf("private=%q: only the visibility query may reach the forge (once per run), got:\n%s", private, calls)
		}
	}
}
