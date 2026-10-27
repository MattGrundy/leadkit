package main

// End-to-end, adversarial coverage of `leadkit brief` through the real binary via the
// mandatory runCLI sandbox. The routing store is a shared t.TempDir (--routing-dir) that
// both the sender and the recipient sessions act on; each "session" is a DISTINCT profile
// whose project_id fixes the slug it resolves to (the same slug refreshPresence derives),
// so the role guards are exercised by real, un-spoofable identities — no brief command
// accepts an actor/slug override, which is the whole security point.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/usage"
)

// briefProject writes a leadkit.yaml whose project_id fixes this "session's" slug, plus
// the store/queue/issues/team layout `--as-task` materialization needs, and returns the
// profile path. A canonical agent type (devops-agent) is seeded so --task-type resolves.
func briefProject(t *testing.T, projectID string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"tasks", "issues", filepath.Join("team", "agents")} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "team", "agents", "devops-agent.md"), []byte("---\nname: devops-agent\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profPath := filepath.Join(root, "leadkit.yaml")
	yaml := "project_id: " + projectID + "\nqueue_root: queue\nstore_root: tasks\nissues_root: issues\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return profPath
}

// runBriefAs runs a brief command as the session owning profPath. The session runs FROM
// its own project directory (cmd.Dir = the profile's dir), so the cwd-honest IDENTITY
// resolver derives the acting slug from that project's OWN leadkit.yaml — the real way a
// session runs (from inside its project), and what makes cross-session addressing honest.
func runBriefAs(t *testing.T, bin, profPath string, args ...string) (string, int) {
	t.Helper()
	return runCLIDir(t, bin, filepath.Dir(profPath), []string{"LEADKIT_PROFILE=" + profPath}, args...)
}

// runCLIDir is runCLI with an explicit session cwd (dir) instead of the bare sandbox
// home, so IDENTITY resolves from dir's own leadkit.yaml. HOME stays sandboxed to a fresh
// temp — the ~-relative-default isolation runCLI documents is preserved.
func runCLIDir(t *testing.T, bin, dir string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = sandboxedEnv(home, extraEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %s %v: %v\n%s", bin, args, err, out)
	return "", -1
}

// sendBriefID extracts the minted id from a `brief sent: <id> -> ...` line.
func sendBriefID(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "brief sent: ") {
			return strings.Fields(line)[2]
		}
	}
	t.Fatalf("no `brief sent:` line in output:\n%s", out)
	return ""
}

// bodyFile writes content to a temp file and returns its path.
func bodyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// sendKnownBrief is the common setup: registers recv, sends a brief sender->recv, returns
// (routingDir, id).
func sendKnownBrief(t *testing.T, bin, senderProf string, subject, body string) (routingDir, id string) {
	t.Helper()
	routingDir = t.TempDir()
	projectsDir := t.TempDir()
	presenceDir := t.TempDir()
	if err := usage.WriteProjectEntry(projectsDir, "recv", "recv", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, code := runBriefAs(t, bin, senderProf, "brief", "send", "--to", "recv",
		"--subject", subject, "--body-file", bodyFile(t, body),
		"--routing-dir", routingDir, "--projects-dir", projectsDir, "--presence-dir", presenceDir)
	if code != 0 {
		t.Fatalf("send must succeed, got %d:\n%s", code, out)
	}
	return routingDir, sendBriefID(t, out)
}

func TestCLI_BriefSend_AddressingAndDeliveryHint(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	routingDir := t.TempDir()
	projectsDir := t.TempDir()
	presenceDir := t.TempDir()
	if err := usage.WriteProjectEntry(projectsDir, "recv", "recv", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := usage.WritePresenceEntry(presenceDir, "recv", "recv-live-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	common := []string{"--routing-dir", routingDir, "--projects-dir", projectsDir, "--presence-dir", presenceDir}

	// Unknown recipient → refused (addressing gate), nothing filed.
	out, code := runBriefAs(t, bin, sender, append([]string{"brief", "send", "--to", "nope", "--subject", "x", "--body-file", bodyFile(t, "b")}, common...)...)
	if code == 0 {
		t.Fatalf("send to an unregistered slug must be refused:\n%s", out)
	}

	// Known recipient + fresh presence → created + delivery hint names the live session.
	out, code = runBriefAs(t, bin, sender, append([]string{"brief", "send", "--to", "recv", "--subject", "the brief", "--body-file", bodyFile(t, "b")}, common...)...)
	if code != 0 || !strings.Contains(out, "recv-live-1") || !strings.Contains(out, "nudge:") {
		t.Fatalf("send must succeed with a live-session hint:\n%s", out)
	}
	// from_slug is the sender's OWN resolved slug.
	items, _ := routing.Open(routingDir).Inbox("recv", routing.InboxFilter{})
	if len(items) != 1 || items[0].FromSlug != "sender" || items[0].Status != "sent" {
		t.Fatalf("brief must be filed as sender->recv status sent: %+v", items)
	}
}

func TestCLI_BriefLifecycleHappyPath(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	routingDir, id := sendKnownBrief(t, bin, sender, "do the thing", "brief body")
	rd := []string{"--routing-dir", routingDir}

	// recv ack -> acked
	if out, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...); code != 0 || !strings.Contains(out, "acked") {
		t.Fatalf("ack must succeed:\n%s", out)
	}
	// recv outcome -> outcome
	if out, code := runBriefAs(t, bin, recv, append([]string{"brief", "outcome", id, "--body-file", bodyFile(t, "shipped it")}, rd...)...); code != 0 || !strings.Contains(out, "outcome recorded") {
		t.Fatalf("outcome must succeed:\n%s", out)
	}
	// sender confirm -> confirmed
	if out, code := runBriefAs(t, bin, sender, append([]string{"brief", "confirm", id, "--note", "lgtm"}, rd...)...); code != 0 || !strings.Contains(out, "confirmed") {
		t.Fatalf("confirm must succeed:\n%s", out)
	}
	// either party close -> closed (terminal)
	if out, code := runBriefAs(t, bin, sender, append([]string{"brief", "close", id}, rd...)...); code != 0 || !strings.Contains(out, "terminal") {
		t.Fatalf("close must succeed:\n%s", out)
	}
	env, err := routing.Open(routingDir).Locate("brief", id)
	if err != nil || env.Status != "closed" || !env.Terminal() {
		t.Fatalf("final state must be closed+terminal: %+v err=%v", env, err)
	}
}

func TestCLI_BriefRoleGuards_SpoofedSlugCannotAct(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	intruder := briefProject(t, "intruder")
	routingDir, id := sendKnownBrief(t, bin, sender, "s", "b")
	rd := []string{"--routing-dir", routingDir}

	// The SENDER cannot ack (recipient-only) — actor slug is its own, not the recipient's.
	if out, code := runBriefAs(t, bin, sender, append([]string{"brief", "ack", id}, rd...)...); code == 0 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("sender must NOT be able to ack:\n%s", out)
	}
	// An unrelated third project cannot ack either (spoof attempt: it is not to_slug).
	if out, code := runBriefAs(t, bin, intruder, append([]string{"brief", "ack", id}, rd...)...); code == 0 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("an unrelated session must NOT be able to ack:\n%s", out)
	}
	// There is no actor-override flag: attempting to inject one is a flag error, never an
	// identity swap.
	if _, code := runBriefAs(t, bin, sender, append([]string{"brief", "ack", id, "--slug", "recv"}, rd...)...); code == 0 {
		t.Fatal("ack must not accept an actor/slug override flag")
	}

	// recv acks legitimately, then the RECIPIENT cannot confirm (sender-only).
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...); code != 0 {
		t.Fatal("recv must be able to ack")
	}
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "outcome", id, "--body-file", bodyFile(t, "r")}, rd...)...); code != 0 {
		t.Fatal("recv must be able to write outcome")
	}
	if out, code := runBriefAs(t, bin, recv, append([]string{"brief", "confirm", id}, rd...)...); code == 0 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("recipient must NOT be able to confirm:\n%s", out)
	}
	// The recipient cannot withdraw (sender-only) either.
	if out, code := runBriefAs(t, bin, recv, append([]string{"brief", "withdraw", id}, rd...)...); code == 0 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("recipient must NOT be able to withdraw:\n%s", out)
	}
}

func TestCLI_BriefWrongOrderRefused(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	routingDir, id := sendKnownBrief(t, bin, sender, "s", "b")
	rd := []string{"--routing-dir", routingDir}

	// recv acks (status now acked). Sender confirm BEFORE any outcome → refused: confirm
	// expects prior status "outcome", finds "acked" → substrate ErrConflict.
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...); code != 0 {
		t.Fatal("ack must succeed")
	}
	out, code := runBriefAs(t, bin, sender, append([]string{"brief", "confirm", id}, rd...)...)
	if code == 0 {
		t.Fatalf("confirm before outcome must be refused:\n%s", out)
	}
	if !strings.Contains(out, "re-check") {
		t.Fatalf("a wrong-order/raced flip must print the re-check message:\n%s", out)
	}
	// The status is untouched by the refused transition.
	env, _ := routing.Open(routingDir).Locate("brief", id)
	if env.Status != "acked" {
		t.Fatalf("a refused transition must not change status, got %q", env.Status)
	}
}

func TestCLI_BriefAsTaskMaterialization(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	recvRoot := filepath.Dir(recv)
	routingDir, id := sendKnownBrief(t, bin, sender, "ship the widget", "b")
	rd := []string{"--routing-dir", routingDir}

	// ack --as-task materializes a linked task in the RECIPIENT's queue carrying brief_id.
	out, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id, "--as-task", "--task-type", "devops-agent"}, rd...)...)
	if code != 0 || !strings.Contains(out, "as-task: created") {
		t.Fatalf("ack --as-task must materialize a task:\n%s", out)
	}
	entries, _ := os.ReadDir(filepath.Join(recvRoot, "tasks"))
	var taskFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			taskFile = e.Name()
		}
	}
	if taskFile == "" {
		t.Fatalf("no task file materialized in the recipient store: %v", entries)
	}
	data, _ := os.ReadFile(filepath.Join(recvRoot, "tasks", taskFile))
	if !strings.Contains(string(data), "brief_id: "+id) {
		t.Fatalf("materialized task must carry brief_id back-link:\n%s", data)
	}
	if !strings.Contains(string(data), "status: pending") || !strings.Contains(string(data), "ship the widget") {
		t.Fatalf("materialized task must be pending + reference the subject:\n%s", data)
	}

	// Without --as-task: no task is created, the brief is otherwise identical.
	sender2 := briefProject(t, "sender")
	recv2 := briefProject(t, "recv")
	recv2Root := filepath.Dir(recv2)
	routingDir2, id2 := sendKnownBrief(t, bin, sender2, "no task", "b")
	if _, code := runBriefAs(t, bin, recv2, "brief", "ack", id2, "--routing-dir", routingDir2); code != 0 {
		t.Fatal("plain ack must succeed")
	}
	entries2, _ := os.ReadDir(filepath.Join(recv2Root, "tasks"))
	for _, e := range entries2 {
		if strings.HasSuffix(e.Name(), ".md") {
			t.Fatalf("plain ack must create NO task, found %s", e.Name())
		}
	}
	// --as-task without --task-type is refused (can't guess a canonical type).
	if _, code := runBriefAs(t, bin, recv2, "brief", "ack", id2, "--as-task", "--routing-dir", routingDir2); code == 0 {
		t.Fatal("--as-task without --task-type must be refused")
	}
}

func TestCLI_BriefOutcomeBodyRenderedByShow(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	routingDir, id := sendKnownBrief(t, bin, sender, "subj", "the original brief prose")
	rd := []string{"--routing-dir", routingDir}

	runBriefAs(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...)
	runBriefAs(t, bin, recv, append([]string{"brief", "outcome", id, "--body-file", bodyFile(t, "the delivered outcome prose")}, rd...)...)

	out, code := runBriefAs(t, bin, recv, append([]string{"brief", "show", id}, rd...)...)
	if code != 0 {
		t.Fatalf("show exit=%d:\n%s", code, out)
	}
	for _, want := range []string{"the original brief prose", "the delivered outcome prose", "--- outcome ---", "--- history ---"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show must render %q:\n%s", want, out)
		}
	}
	// History is the ordered thread with actor stamps: created -> acked -> outcome.
	iCreated := strings.Index(out, "created")
	iAcked := strings.Index(out, "acked")
	iOutcome := strings.LastIndex(out, "outcome")
	if !(iCreated < iAcked && iAcked < iOutcome) {
		t.Fatalf("history must be ordered created<acked<outcome:\n%s", out)
	}
	if !strings.Contains(out, "by recv") || !strings.Contains(out, "by sender") {
		t.Fatalf("history must stamp the acting slugs:\n%s", out)
	}
}

func TestCLI_BriefDeclineAndWithdrawOffRamps(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")

	// decline: recv refuses a sent brief; sender then closes it (declined is non-terminal).
	routingDir, id := sendKnownBrief(t, bin, sender, "s", "b")
	rd := []string{"--routing-dir", routingDir}
	if out, code := runBriefAs(t, bin, recv, append([]string{"brief", "decline", id, "--note", "not us"}, rd...)...); code != 0 || !strings.Contains(out, "declined") {
		t.Fatalf("decline must succeed:\n%s", out)
	}
	if out, code := runBriefAs(t, bin, sender, append([]string{"brief", "close", id}, rd...)...); code != 0 {
		t.Fatalf("a declined brief must be closeable:\n%s", out)
	}

	// withdraw: sender pulls a non-terminal brief → terminal; further transitions refused.
	routingDir2, id2 := sendKnownBrief(t, bin, sender, "s2", "b2")
	rd2 := []string{"--routing-dir", routingDir2}
	if out, code := runBriefAs(t, bin, sender, append([]string{"brief", "withdraw", id2}, rd2...)...); code != 0 || !strings.Contains(out, "terminal") {
		t.Fatalf("withdraw must succeed and be terminal:\n%s", out)
	}
	// A withdrawn (terminal) brief refuses further transitions.
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id2}, rd2...)...); code == 0 {
		t.Fatal("a withdrawn brief must refuse an ack")
	}
	if _, code := runBriefAs(t, bin, sender, append([]string{"brief", "withdraw", id2}, rd2...)...); code == 0 {
		t.Fatal("a withdrawn brief must refuse a second withdraw")
	}
}

func TestCLI_BriefErrConflictSurfacing(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	routingDir, id := sendKnownBrief(t, bin, sender, "s", "b")
	rd := []string{"--routing-dir", routingDir}

	// recv acks. Now mutate the store out-of-band to simulate a concurrent flip so recv's
	// next ack sees a stale prior status → ErrConflict re-check message + non-zero exit.
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...); code != 0 {
		t.Fatal("ack must succeed")
	}
	// A second ack now expects "sent" but finds "acked" → conflict.
	out, code := runBriefAs(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...)
	if code == 0 {
		t.Fatalf("a stale ack must exit non-zero:\n%s", out)
	}
	if !strings.Contains(out, "re-check") || strings.Contains(out, "force") {
		t.Fatalf("ErrConflict must surface a re-check message and never force:\n%s", out)
	}
}

// TestCLI_BriefInbox_UnionsLedWorkstreams exercises the registry-led addressing + inbox
// union end to end through the real binary: a lead whose cwd slug is `helm-chart-cache`
// registers `--leads legacy-migration`; a brief addressed to the WORKSTREAM name is
// filed under legacy-migration (storage unchanged, from= the sender's own slug); and
// the lead's `brief inbox` (no --slug) sees BOTH its own-slug inbox and the led
// workstream's — while the same inbox WITHOUT the registry shows own-slug only.
func TestCLI_BriefInbox_UnionsLedWorkstreams(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	helm := briefProject(t, "helm-chart-cache")

	routingDir := t.TempDir()
	registryRepo := t.TempDir()
	projectsDir := t.TempDir()
	// The lead's own project IS a registered project (so it can be addressed by its own
	// slug); legacy-migration is NOT a project — it is addressable only via the registry.
	if err := usage.WriteProjectEntry(projectsDir, "helm-chart-cache", "helm", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}

	// helm (cwd slug helm-chart-cache) registers as the lead of legacy-migration.
	helmEnv := []string{"LEADKIT_PROFILE=" + helm, "LEADKIT_SESSION_ID=helm-sess"}
	if out, code := runCLIDir(t, bin, filepath.Dir(helm), helmEnv,
		"register", "--role", "lead", "--leads", "legacy-migration", "--registry-repo", registryRepo); code != 0 {
		t.Fatalf("register must succeed:\n%s", out)
	}

	common := []string{"--routing-dir", routingDir, "--projects-dir", projectsDir,
		"--presence-dir", t.TempDir(), "--registry-repo", registryRepo}
	senderEnv := []string{"LEADKIT_PROFILE=" + sender, "LEADKIT_SESSION_ID=sender-sess"}

	// A brief to the WORKSTREAM name (addressable via the live registry lead).
	outW, codeW := runCLIDir(t, bin, filepath.Dir(sender), senderEnv,
		append([]string{"brief", "send", "--to", "legacy-migration", "--subject", "workstream brief", "--body-file", bodyFile(t, "b")}, common...)...)
	if codeW != 0 {
		t.Fatalf("send to a registry-led workstream must succeed:\n%s", outW)
	}
	wsID := sendBriefID(t, outW)

	// A brief to the lead's OWN slug (a registered project).
	outO, codeO := runCLIDir(t, bin, filepath.Dir(sender), senderEnv,
		append([]string{"brief", "send", "--to", "helm-chart-cache", "--subject", "own-slug brief", "--body-file", bodyFile(t, "b")}, common...)...)
	if codeO != 0 {
		t.Fatalf("send to the own slug must succeed:\n%s", outO)
	}
	ownID := sendBriefID(t, outO)

	// STORAGE UNCHANGED: the workstream brief is filed under legacy-migration, from=sender.
	wsItems, _ := routing.Open(routingDir).Inbox("legacy-migration", routing.InboxFilter{Kind: "brief"})
	if len(wsItems) != 1 || wsItems[0].ID != wsID || wsItems[0].FromSlug != "sender" {
		t.Fatalf("workstream brief must be stored under legacy-migration with from=sender: %+v", wsItems)
	}

	// INBOX UNION: helm's inbox (no --slug) WITH the registry sees BOTH inboxes.
	outU, codeU := runCLIDir(t, bin, filepath.Dir(helm), helmEnv,
		"brief", "inbox", "--routing-dir", routingDir, "--registry-repo", registryRepo)
	if codeU != 0 || !strings.Contains(outU, wsID) || !strings.Contains(outU, ownID) {
		t.Fatalf("inbox must union own-slug + led-workstream inboxes:\n%s", outU)
	}

	// NO REGISTRY (led lookup finds nothing): own-slug only — the workstream brief is hidden.
	outOwn, codeOwn := runCLIDir(t, bin, filepath.Dir(helm), helmEnv,
		"brief", "inbox", "--routing-dir", routingDir)
	if codeOwn != 0 || !strings.Contains(outOwn, ownID) {
		t.Fatalf("own-slug inbox must still show the own-slug brief:\n%s", outOwn)
	}
	if strings.Contains(outOwn, wsID) {
		t.Fatalf("without the registry, a led-workstream brief must NOT appear in the own-slug inbox:\n%s", outOwn)
	}
}

func TestCLI_BriefInbox_HidesTerminalByDefault(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")

	// Two briefs to recv on one store: one live (sent), one withdrawn (terminal).
	routingDir, liveID := sendKnownBrief(t, bin, sender, "live one", "b")
	rd := []string{"--routing-dir", routingDir}
	// Send a second on the SAME store by reusing the routing dir directly.
	projectsDir := t.TempDir()
	if err := usage.WriteProjectEntry(projectsDir, "recv", "recv", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, _ := runBriefAs(t, bin, sender, "brief", "send", "--to", "recv", "--subject", "gone one",
		"--body-file", bodyFile(t, "b"), "--routing-dir", routingDir, "--projects-dir", projectsDir, "--presence-dir", t.TempDir())
	termID := sendBriefID(t, out)
	if _, code := runBriefAs(t, bin, sender, append([]string{"brief", "withdraw", termID}, rd...)...); code != 0 {
		t.Fatal("withdraw must succeed")
	}

	// Default inbox: live shown, terminal hidden.
	out, code := runBriefAs(t, bin, recv, append([]string{"brief", "inbox"}, rd...)...)
	if code != 0 || !strings.Contains(out, liveID) || strings.Contains(out, termID) {
		t.Fatalf("default inbox must show live + hide terminal:\n%s", out)
	}
	// --all-status includes the terminal one.
	out, code = runBriefAs(t, bin, recv, append([]string{"brief", "inbox", "--all-status"}, rd...)...)
	if code != 0 || !strings.Contains(out, termID) || !strings.Contains(out, liveID) {
		t.Fatalf("--all-status must include terminal briefs:\n%s", out)
	}
}

// sendBriefTo files a brief sender->to on an existing routing store (registering `to` so
// the addressing gate passes) and returns its id.
func sendBriefTo(t *testing.T, bin, senderProf, routingDir, to, subject string) string {
	t.Helper()
	projectsDir := t.TempDir()
	if err := usage.WriteProjectEntry(projectsDir, to, to, "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, code := runBriefAs(t, bin, senderProf, "brief", "send", "--to", to, "--subject", subject,
		"--body-file", bodyFile(t, "b"), "--routing-dir", routingDir, "--projects-dir", projectsDir, "--presence-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("send to %s must succeed, got %d:\n%s", to, code, out)
	}
	return sendBriefID(t, out)
}

// mustBrief runs a brief verb and fails the test unless it exits 0.
func mustBrief(t *testing.T, bin, prof string, args ...string) string {
	t.Helper()
	out, code := runBriefAs(t, bin, prof, args...)
	if code != 0 {
		t.Fatalf("%v must succeed, got %d:\n%s", args, code, out)
	}
	return out
}

// TestCLI_BriefAwaiting_ListsOnlyParked is the discriminating test for §2.1: a brief in
// EVERY other lifecycle state sits in the same store, and `brief awaiting` must report
// the parked ones and NONE of the rest. A view that listed every brief would pass a
// parked-only fixture; this one fails it.
func TestCLI_BriefAwaiting_ListsOnlyParked(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	recv2 := briefProject(t, "recvtwo")
	routingDir := t.TempDir()
	rd := []string{"--routing-dir", routingDir}
	send := func(to, subj string) string { return sendBriefTo(t, bin, sender, routingDir, to, subj) }

	// One brief per non-parked lifecycle state.
	idSent := send("recv", "state-sent")

	idAcked := send("recv", "state-acked")
	mustBrief(t, bin, recv, append([]string{"brief", "ack", idAcked}, rd...)...)

	idOutcome := send("recv", "state-outcome")
	mustBrief(t, bin, recv, append([]string{"brief", "ack", idOutcome}, rd...)...)
	mustBrief(t, bin, recv, append([]string{"brief", "outcome", idOutcome, "--body-file", bodyFile(t, "done")}, rd...)...)

	idConfirmed := send("recv", "state-confirmed")
	mustBrief(t, bin, recv, append([]string{"brief", "ack", idConfirmed}, rd...)...)
	mustBrief(t, bin, recv, append([]string{"brief", "outcome", idConfirmed, "--body-file", bodyFile(t, "done")}, rd...)...)
	mustBrief(t, bin, sender, append([]string{"brief", "confirm", idConfirmed}, rd...)...)

	idClosed := send("recv", "state-closed")
	mustBrief(t, bin, recv, append([]string{"brief", "ack", idClosed}, rd...)...)
	mustBrief(t, bin, recv, append([]string{"brief", "outcome", idClosed, "--body-file", bodyFile(t, "done")}, rd...)...)
	mustBrief(t, bin, sender, append([]string{"brief", "confirm", idClosed}, rd...)...)
	mustBrief(t, bin, sender, append([]string{"brief", "close", idClosed}, rd...)...)

	idDeclined := send("recv", "state-declined")
	mustBrief(t, bin, recv, append([]string{"brief", "decline", idDeclined, "--note", "no"}, rd...)...)

	idWithdrawn := send("recv", "state-withdrawn")
	mustBrief(t, bin, sender, append([]string{"brief", "withdraw", idWithdrawn}, rd...)...)

	// Two parked briefs, to DIFFERENT recipients, one parked from sent and one from acked:
	// the owner's queue must span recipients.
	idParkedSent := send("recv", "state-parked-from-sent")
	mustBrief(t, bin, recv, append([]string{"brief", "await-user", idParkedSent, "--note", "which cluster?"}, rd...)...)

	idParkedAcked := send("recvtwo", "state-parked-from-acked")
	mustBrief(t, bin, recv2, append([]string{"brief", "ack", idParkedAcked}, rd...)...)
	mustBrief(t, bin, recv2, append([]string{"brief", "await-user", idParkedAcked, "--note", "prod or staging?"}, rd...)...)

	out := mustBrief(t, bin, sender, append([]string{"brief", "awaiting"}, rd...)...)
	for _, want := range []string{idParkedSent, idParkedAcked, "which cluster?", "prod or staging?", "to=recv ", "to=recvtwo "} {
		if !strings.Contains(out, want) {
			t.Errorf("awaiting must report %q:\n%s", want, out)
		}
	}
	for name, id := range map[string]string{
		"sent": idSent, "acked": idAcked, "outcome": idOutcome, "confirmed": idConfirmed,
		"closed": idClosed, "declined": idDeclined, "withdrawn": idWithdrawn,
	} {
		if strings.Contains(out, id) {
			t.Errorf("a %s brief is NOT awaiting-user and must not be reported:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "(2,") {
		t.Errorf("exactly the two parked briefs must be counted:\n%s", out)
	}
}

func TestCLI_BriefAwaiting_EmptyWhenNothingParked(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	routingDir, id := sendKnownBrief(t, bin, sender, "live but not parked", "b")
	out := mustBrief(t, bin, sender, "brief", "awaiting", "--routing-dir", routingDir)
	if strings.Contains(out, id) || !strings.Contains(out, "nothing is waiting") {
		t.Fatalf("an unparked store must report nothing waiting:\n%s", out)
	}
}

func TestCLI_BriefAwaitUser_ParkAndResume(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	routingDir, id := sendKnownBrief(t, bin, sender, "needs the owner", "b")
	rd := []string{"--routing-dir", routingDir}
	status := func() string {
		env, err := routing.Open(routingDir).Locate("brief", id)
		if err != nil {
			t.Fatal(err)
		}
		return env.Status
	}

	// Parking records the status, is NOT terminal, and prints a nudge naming the sender.
	out := mustBrief(t, bin, recv, append([]string{"brief", "await-user", id, "--note", "clarify scope"}, rd...)...)
	if status() != "awaiting-user" || !strings.Contains(out, "sender") {
		t.Fatalf("park must set awaiting-user and hint at the sender (status=%q):\n%s", status(), out)
	}
	env, _ := routing.Open(routingDir).Locate("brief", id)
	if env.Terminal() {
		t.Fatal("a parked brief must stay non-terminal")
	}
	// A parked brief cannot be re-parked, and cannot skip to an outcome.
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "await-user", id, "--note", "again"}, rd...)...); code == 0 {
		t.Fatal("re-parking a parked brief must be refused")
	}
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "outcome", id, "--body-file", bodyFile(t, "x")}, rd...)...); code == 0 || status() != "awaiting-user" {
		t.Fatalf("a parked brief must not reach outcome without resuming (status=%q)", status())
	}

	// Resume restores the ORIGINAL status: parked from `sent`, it is ackable again.
	mustBrief(t, bin, recv, append([]string{"brief", "resume", id}, rd...)...)
	if status() != "sent" {
		t.Fatalf("a brief parked before ack must resume to sent, got %q", status())
	}
	mustBrief(t, bin, recv, append([]string{"brief", "ack", id}, rd...)...)

	// Parked from `acked` resumes to `acked`.
	mustBrief(t, bin, recv, append([]string{"brief", "await-user", id, "--note", "second question"}, rd...)...)
	mustBrief(t, bin, recv, append([]string{"brief", "resume", id, "--note", "owner said prod"}, rd...)...)
	if status() != "acked" {
		t.Fatalf("a brief parked after ack must resume to acked, got %q", status())
	}
	// A resumed brief must leave the awaiting queue: the owner's list holds only what is still parked.
	out = mustBrief(t, bin, sender, append([]string{"brief", "awaiting"}, rd...)...)
	if strings.Contains(out, id) {
		t.Fatalf("a resumed brief must leave the awaiting queue:\n%s", out)
	}
	// Resume only applies to a parked brief.
	if out, code := runBriefAs(t, bin, recv, append([]string{"brief", "resume", id}, rd...)...); code == 0 || !strings.Contains(out, "only a \"awaiting-user\" brief may be resumed") {
		t.Fatalf("resuming a brief that is not parked must be refused with the legality message (code=%d):\n%s", code, out)
	}
}

func TestCLI_BriefAwaitUser_RoleGuardsAndNoteRequired(t *testing.T) {
	bin := testBinary(t)
	sender := briefProject(t, "sender")
	recv := briefProject(t, "recv")
	stranger := briefProject(t, "stranger")
	routingDir, id := sendKnownBrief(t, bin, sender, "guarded", "b")
	rd := []string{"--routing-dir", routingDir}
	status := func() string { e, _ := routing.Open(routingDir).Locate("brief", id); return e.Status }

	// Only the RECIPIENT may park: not the sender, not a third party.
	for name, prof := range map[string]string{"sender": sender, "stranger": stranger} {
		if out, code := runBriefAs(t, bin, prof, append([]string{"brief", "await-user", id, "--note", "x"}, rd...)...); code == 0 || !strings.Contains(out, "REFUSED") {
			t.Errorf("%s must be refused from parking (code=%d):\n%s", name, code, out)
		}
	}
	// A note is mandatory: it is what the owner reads.
	if _, code := runBriefAs(t, bin, recv, append([]string{"brief", "await-user", id}, rd...)...); code != 2 {
		t.Errorf("await-user without --note must be a usage error, got %d", code)
	}
	if status() != "sent" {
		t.Fatalf("refused attempts must not change status, got %q", status())
	}

	// Only the recipient may resume a parked brief.
	mustBrief(t, bin, recv, append([]string{"brief", "await-user", id, "--note", "q"}, rd...)...)
	for name, prof := range map[string]string{"sender": sender, "stranger": stranger} {
		if _, code := runBriefAs(t, bin, prof, append([]string{"brief", "resume", id}, rd...)...); code == 0 {
			t.Errorf("%s must be refused from resuming", name)
		}
	}
	if status() != "awaiting-user" {
		t.Fatalf("refused resumes must leave the brief parked, got %q", status())
	}
	// The sender can still withdraw a parked brief (any non-terminal state).
	mustBrief(t, bin, sender, append([]string{"brief", "withdraw", id}, rd...)...)
}
