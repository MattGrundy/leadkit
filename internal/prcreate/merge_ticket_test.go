package prcreate

import (
	"reflect"
	"strings"
	"testing"
)

// Every merge this verb makes is marked `Leadkit-Merge: pr-merge`, so a merge commit on the base
// branch without it was made some other way (ticket 289 reads that absence). `Ticket: N` is added
// only when a ticket is named, and its absence means only "no ticket linked". The argv is where both
// are pinned, because it is the message of the commit the forge is about to create.

const marker = "Leadkit-Merge: pr-merge"

func TestDecideMerge_Marker_SquashWithNoTicketIsMarkedButCarriesNoTicket(t *testing.T) {
	d := DecideMerge(mergeableInput())
	if !d.Allowed {
		t.Fatalf("refused: %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets",
		"--squash", "--subject", "T (#77)", "--body", "* fix: the thing\n\n" + marker, "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv:\n got %q\nwant %q", d.Argv, want)
	}
	if strings.Contains(strings.Join(d.Argv, " "), "Ticket:") {
		t.Fatalf("no ticket was named, so no Ticket line may be written: %q", d.Argv)
	}
}

func TestDecideMerge_Marker_MergeCommitWithNoTicketIsMarkedAndInheritsNothingElse(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "merge"
	in.Body = "* a composed squash body that a merge commit must not inherit"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("refused: %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets", "--merge", "--body", marker, "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv:\n got %q\nwant %q", d.Argv, want)
	}
}

func TestDecideMerge_Ticket_SquashAddsTheTicketLineToTheSameBlockAsTheMarker(t *testing.T) {
	in := mergeableInput()
	in.Req.Ticket = "265"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("refused: %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets",
		"--squash", "--subject", "T (#77)", "--body", "* fix: the thing\n\n" + marker + "\nTicket: 265", "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv:\n got %q\nwant %q", d.Argv, want)
	}
}

func TestDecideMerge_Ticket_MergeCommitCarriesOnlyTheTrailerBlock(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "merge"
	in.Body = "* a composed squash body that a merge commit must not inherit"
	in.Req.Ticket = "265"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("refused: %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets", "--merge", "--body", marker + "\nTicket: 265", "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv:\n got %q\nwant %q", d.Argv, want)
	}
}

func TestDecideMerge_Ticket_StackedDefaultToMergeStillCarriesIt(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "" // none asked for: the stack check defaults to merge
	in.Dependents = []string{"feat/stacked"}
	in.Req.Ticket = "265"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("refused: %s", d.Reason)
	}
	got := strings.Join(d.Argv, " ")
	if !strings.Contains(got, "--merge") || !strings.Contains(got, "--body "+marker+"\nTicket: 265") || strings.Contains(got, "--subject") {
		t.Fatalf("argv: %q", d.Argv)
	}
}

func TestDecideMerge_Rebase_HasNoCommitToMarkSoAnUnticketedOneProceedsUnmarked(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "rebase"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("a rebase asked for with no ticket is allowed, as before: %s", d.Reason)
	}
	if strings.Contains(strings.Join(d.Argv, " "), "Leadkit-Merge") {
		t.Fatalf("a rebase writes no commit, so there is nothing to put the marker in: %q", d.Argv)
	}
}

func TestDecideMerge_Ticket_RebaseIsRefusedNotMergedUnlinked(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "rebase"
	in.Req.Ticket = "265"
	d := DecideMerge(in)
	if d.Allowed || d.Argv != nil || !strings.Contains(d.Reason, "rebase writes no commit") {
		t.Fatalf("a rebase cannot carry the trailer and must be refused, got %+v", d)
	}
	in.UserApproved = true
	if d := DecideMerge(in); d.Allowed {
		t.Fatalf("--user-approved vouches for review, not for a trailer that cannot be written: %+v", d)
	}
}

func TestDecideMerge_GitLab(t *testing.T) {
	in := mergeableInput()
	in.OriginURL = "git@gitlab.com:acmecorp/widgets.git"
	in.Req.Ticket = "265"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("refused: %s", d.Reason)
	}
	want := []string{"glab", "mr", "merge", "77", "--repo", "acmecorp/widgets", "--squash", "--message", "T (#77)\n\n" + marker + "\nTicket: 265", "--sha", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv:\n got %q\nwant %q", d.Argv, want)
	}

	in.Req.Method = "merge"
	if d := DecideMerge(in); d.Allowed || !strings.Contains(d.Reason, "gitlab") {
		t.Fatalf("a gitlab merge commit cannot be told its message, so a ticket-linked one is refused: %+v", d)
	}
	in.Req.Ticket = ""
	if d := DecideMerge(in); !d.Allowed {
		t.Fatalf("an unticketed gitlab merge commit proceeds, unmarked, as it did before: %s", d.Reason)
	}
}
