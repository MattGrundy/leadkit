package hook

import "testing"

func backstopCfg(blocked bool) MilestoneBackstopConfig {
	return MilestoneBackstopConfig{
		Evaluate: func() (bool, string) {
			return blocked, "milestone m1: 3 open task(s) + 2 unmet/unverified signal(s)"
		},
	}
}

func sendMsg(text string) Input {
	return Input{ToolName: "SendMessage", ToolInput: toolInput{Message: text}}
}

// A work-at-rest claim + a would-block milestone → WARN (stderr, exit 0), NEVER a hard
// block.
func TestMilestoneBackstop_WarnsOnRestClaimWhenBlocked(t *testing.T) {
	res := MilestoneBackstop(sendMsg("The milestone is complete — nothing needs you; the team is done and at rest."), backstopCfg(true))
	if res.ExitCode != 0 {
		t.Fatalf("backstop must NEVER hard-block a user message; exit = %d, want 0", res.ExitCode)
	}
	if res.Stderr == "" {
		t.Error("a blocked would-be-idle claim should produce a WARNING on stderr")
	}
}

// A clean milestone → silent, even on the same rest-claim phrasing.
func TestMilestoneBackstop_SilentWhenNotBlocked(t *testing.T) {
	res := MilestoneBackstop(sendMsg("The milestone is complete — nothing needs you."), backstopCfg(false))
	if res.ExitCode != 0 || res.Stderr != "" {
		t.Errorf("clean milestone must be silent, got %+v", res)
	}
}

// A non-idle message → untouched (no rest claim at all).
func TestMilestoneBackstop_UntouchedOnNonIdleMessage(t *testing.T) {
	res := MilestoneBackstop(sendMsg("Opened PR #42 for the extraction; please review when you can."), backstopCfg(true))
	if res.ExitCode != 0 || res.Stderr != "" {
		t.Errorf("a non-idle message must be untouched, got %+v", res)
	}
}

// FOUNDING PRINCIPLE: the lead may idle; the team may not. A lead announcing its OWN
// pause WITH work in flight passes SILENTLY even when the milestone would block — the
// backstop keys on WORK-is-at-rest phrasing, not on the lead reporting it is pausing.
func TestMilestoneBackstop_LeadPauseWithWorkInFlightPassesSilently(t *testing.T) {
	msg := "We're at a rest point for me personally — I'll idle now, awaiting the builder/gate/CI to report back."
	res := MilestoneBackstop(sendMsg(msg), backstopCfg(true))
	if res.ExitCode != 0 || res.Stderr != "" {
		t.Errorf("a lead-pause-with-work-in-flight message must pass silently even when the milestone would block, got %+v", res)
	}
}

// Not a SendMessage, or no evaluator wired → fail-open allow.
func TestMilestoneBackstop_FailsOpen(t *testing.T) {
	if res := MilestoneBackstop(Input{ToolName: "Bash", ToolInput: toolInput{Command: "milestone is complete"}}, backstopCfg(true)); res.ExitCode != 0 || res.Stderr != "" {
		t.Errorf("non-SendMessage must be allowed silently, got %+v", res)
	}
	if res := MilestoneBackstop(sendMsg("the milestone is complete, nothing needs you"), MilestoneBackstopConfig{}); res.ExitCode != 0 || res.Stderr != "" {
		t.Errorf("no evaluator wired must be allowed silently, got %+v", res)
	}
}
