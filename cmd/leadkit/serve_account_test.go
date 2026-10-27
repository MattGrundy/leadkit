package main

import "testing"

// TestServeAccountMode_ExplicitQueueStaysSingleProject is
// multi-project-board.md's hard-constraint regression at the flag-decision
// level: whenever -queue is explicitly given (and neither -account nor -all
// is), runServe must take the single-project path — the one that builds a
// plain *viewer.Server and never references the account project registry —
// exactly as it always has.
func TestServeAccountMode_ExplicitQueueStaysSingleProject(t *testing.T) {
	if serveAccountMode("/some/queue/root", false, false) {
		t.Error("serveAccountMode(queue-set, no account flags) = true, want false (single-project mode, byte-identical to today)")
	}
}

func TestServeAccountMode_NoQueueDefaultsToAccountMode(t *testing.T) {
	if !serveAccountMode("", false, false) {
		t.Error("serveAccountMode(\"\", false, false) = false, want true (no -queue => ACCOUNT mode per the spec)")
	}
}

func TestServeAccountMode_AccountFlagForcesAccountModeEvenWithQueue(t *testing.T) {
	if !serveAccountMode("/some/queue/root", true, false) {
		t.Error("serveAccountMode(queue-set, -account) = false, want true (-account is an explicit override)")
	}
}

func TestServeAccountMode_AllFlagIsAnAliasForAccount(t *testing.T) {
	if !serveAccountMode("", false, true) {
		t.Error("serveAccountMode(\"\", false, -all) = false, want true")
	}
	if !serveAccountMode("/some/queue/root", false, true) {
		t.Error("serveAccountMode(queue-set, -all) = false, want true (-all is an explicit override, same as -account)")
	}
}
