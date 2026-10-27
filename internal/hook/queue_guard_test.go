package hook

import (
	"strings"
	"testing"
)

func TestQueueGuard_SanctionedPathsAllow(t *testing.T) {
	tests := []string{
		"leadkit queue move queue/cr-1/pending/1.md queue/cr-1/in-progress/1.md",
		"leadkit queue sweep --poke",
		"leadkit queue sweep",
		// The sanction has to bite to mean anything: these clauses carry a raw verb AND a
		// queue path (in an argument), so only the invoked-command rule lets them through.
		`leadkit queue sweep --reason "rm queue/done/stale.md"`,
		`leadkit queue move --dry-run queue/cr-1/pending/rm\ queue/done/`,
		"leadkit-bin queue sweep --note 'rm queue/done/x'",
		`~/.claude/leadkit-bin queue sweep --note "rm queue/done/x"`,
		`"$HOME/.claude/leadkit-bin" queue sweep --note "rm queue/done/x"`,
		"LEADKIT_QUEUE=/q leadkit queue sweep --note 'rm queue/done/x'",
		"cd /repo && leadkit queue sweep --note 'rm queue/done/x'",
	}
	for _, cmd := range tests {
		if got := QueueGuard(Input{ToolInput: toolInput{Command: cmd}}, "queue"); got != allow {
			t.Errorf("QueueGuard(%q) = %+v, want allow (sanctioned path)", cmd, got)
		}
	}
}

// The sanction is the INVOKED command, not a name appearing somewhere in the line. Each of
// these runs a raw rm/mv/ln on the queue and must block.
func TestQueueGuard_MentioningASanctionedNameDoesNotSanction(t *testing.T) {
	tests := []string{
		// assign.sh and the other .sh names do not exist, so naming one sanctions nothing.
		"rm queue/code-review/pending/1-x.md # assign.sh",
		"echo assign.sh; rm queue/code-review/pending/1-x.md",
		"rm queue/code-review/pending/1-x.md",
		"rm queue/code-review/pending/1-x.md # queue-move.sh send-to-review.sh gate-verdict.sh queue-sweep.sh",
		"rm queue/code-review/pending/1-x.md && ~/.claude/queue-move.sh a b",
		"~/.claude/queue-move.sh a b; rm queue/code-review/pending/1-x.md",
		// The same shape with the live sanctioned verb: naming it is not running it.
		"rm queue/code-review/pending/1-x.md # leadkit queue move",
		"echo leadkit queue move; rm queue/code-review/pending/1-x.md",
		"echo 'leadkit queue sweep' && mv queue/cr-1/pending/1.md queue/cr-1/done/",
		"leadkit queue move a b && rm queue/code-review/pending/1-x.md",
		"leadkit queue move a b; mv queue/cr-1/pending/1.md queue/cr-1/done/",
		"leadkit queue move a b || ln -s x queue/cr-1/pending/1.md",
		"leadkit queue sweep | rm queue/done/1.md",
		// A sanctioned mover that runs a raw verb through a substitution is not sanctioned.
		"leadkit queue move a b $(rm queue/done/1.md)",
		"leadkit queue move a b `rm queue/done/1.md`",
		"leadkit queue sweep --x <(rm queue/done/1.md)",
		"X=$(rm queue/done/1.md) leadkit queue sweep",
		// Not the sanctioned subcommands, not the sanctioned binary.
		"leadkit queue rm queue/done/1.md",
		"leadkit queue consolidate rm queue/done/1.md",
		"notleadkit queue move rm queue/done/1.md",
		"sudo leadkit queue move rm queue/done/1.md",
		"bash -c 'rm queue/done/1.md' leadkit queue move",
		"( rm queue/done/1.md ) # leadkit queue move",
	}
	for _, cmd := range tests {
		got := QueueGuard(Input{ToolInput: toolInput{Command: cmd}}, "queue")
		if got.ExitCode != 2 {
			t.Errorf("QueueGuard(%q) = %+v, want a block", cmd, got)
		}
	}
}

// Each substitution form that executes a command is its own arm of the sanctionedClause
// check, so each is pinned by a case that no other arm can catch: the clause is otherwise a
// fully sanctioned `leadkit queue ...` and carries exactly one form. Deleting any single arm
// must fail exactly its own case. `=(` is zsh-only but zsh is the shell in use.
func TestQueueGuard_EachSubstitutionArmBlocksOnItsOwn(t *testing.T) {
	tests := []struct{ arm, cmd string }{
		{"$(", "leadkit queue move a b $(rm queue/done/1.md)"},
		{"`", "leadkit queue move a b `rm queue/done/1.md`"},
		{"<(", "leadkit queue sweep --x <(rm queue/done/1.md)"},
		{">(", "leadkit queue sweep >(rm queue/done/1.md)"},
		{"=(", "leadkit queue move a b =(rm queue/done/1.md)"},
	}
	for _, tc := range tests {
		t.Run(tc.arm, func(t *testing.T) {
			if got := QueueGuard(Input{ToolInput: toolInput{Command: tc.cmd}}, "queue"); got.ExitCode != 2 {
				t.Errorf("QueueGuard(%q) = %+v, want a block (substitution %q runs a raw rm)", tc.cmd, got, tc.arm)
			}
		})
	}
}

func TestQueueGuard_RawMvOnQueueBlocks(t *testing.T) {
	tests := []string{
		"mv queue/cr-1/pending/1.md queue/cr-1/in-progress/1.md",
		"mv legacy-migration/queue/crit-2/pending/1.md legacy-migration/queue/crit-2/done/1.md",
		"ln -s ../../../tasks/1.md queue/cr-1/pending/1.md",
		"rm queue/done/1.md",
	}
	for _, cmd := range tests {
		got := QueueGuard(Input{ToolInput: toolInput{Command: cmd}}, "queue")
		if got.ExitCode != 2 {
			t.Errorf("QueueGuard(%q).ExitCode = %d, want 2", cmd, got.ExitCode)
		}
		if got.Stderr == "" {
			t.Errorf("QueueGuard(%q) has empty Stderr, want guidance", cmd)
		}
	}
}

func TestQueueGuard_UngovernedCommandsAllow(t *testing.T) {
	tests := []string{
		"",
		"mv foo.txt bar.txt",                // mv, but not the queue
		"ls queue/cr-1/pending/",            // no mv/ln/rm token
		"warm queue/cr-1/pending/1.md",      // "rm" inside a word, not a bare token
		"echo firm queue/cr-1/pending/1.md", // same — "rm" not a bare token
	}
	for _, cmd := range tests {
		if got := QueueGuard(Input{ToolInput: toolInput{Command: cmd}}, "queue"); got != allow {
			t.Errorf("QueueGuard(%q) = %+v, want allow", cmd, got)
		}
	}
}

func TestQueueGuard_UsesProfileQueueBase(t *testing.T) {
	// A custom queue_root basename ("myqueue") — the guard must trigger on THAT
	// basename, and a command referencing the conventional "queue/" (a different
	// project's shape) must not falsely trigger.
	blocked := QueueGuard(Input{ToolInput: toolInput{Command: "mv myqueue/cr-1/pending/1.md myqueue/cr-1/done/1.md"}}, "myqueue")
	if blocked.ExitCode != 2 {
		t.Errorf("want a block for the configured queue base, got %+v", blocked)
	}

	notBlocked := QueueGuard(Input{ToolInput: toolInput{Command: "mv queue/cr-1/pending/1.md queue/cr-1/done/1.md"}}, "myqueue")
	if notBlocked != allow {
		t.Errorf("want allow — cmd references a different queue base than configured, got %+v", notBlocked)
	}
}

func TestQueueGuard_EmptyQueueBaseFallsBackToDefault(t *testing.T) {
	got := QueueGuard(Input{ToolInput: toolInput{Command: "mv queue/cr-1/pending/1.md queue/cr-1/done/1.md"}}, "")
	if got.ExitCode != 2 {
		t.Errorf("want a block using the default \"queue\" base, got %+v", got)
	}
}

// TestQueueGuard_BoundaryAndClause covers the two false-positive fixes: the queue base
// must match as a PATH SEGMENT (not a mid-word substring), and the verb + queue path must
// share a SINGLE command clause.
func TestQueueGuard_BoundaryAndClause(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		queueBase string
		wantAllow bool
	}{
		{
			name:      "requeue in commit message + unrelated rm in another clause (the exact false positive)",
			cmd:       `git commit -m "reconcile requeue/route terminal/review" && rm -f /tmp/x`,
			queueBase: "queue",
			wantAllow: true,
		},
		{
			name:      "rm elsewhere, no queue path anywhere",
			cmd:       "rm -rf node_modules && echo done",
			queueBase: "queue",
			wantAllow: true,
		},
		{
			name:      "real raw queue mv, verb + path in the same clause",
			cmd:       "mv queue/cr-1/pending/x.md queue/cr-1/in-progress/",
			queueBase: "queue",
			wantAllow: false,
		},
		{
			name:      "queue read in one clause, rm in another (no queue path with the rm)",
			cmd:       "cat queue/cr-1/pending/x.md && rm /tmp/y",
			queueBase: "queue",
			wantAllow: true,
		},
		{
			name:      "sanctioned leadkit mover still allowed",
			cmd:       "leadkit queue move queue/cr-1/pending/1.md queue/cr-1/in-progress/1.md",
			queueBase: "queue",
			wantAllow: true,
		},
		{
			name:      "mid-word base (dequeue) with a real rm elsewhere",
			cmd:       "echo dequeue/route && rm -f /tmp/z",
			queueBase: "queue",
			wantAllow: true,
		},
		{
			name:      "mid-word base (myqueue) with a real rm elsewhere",
			cmd:       "echo myqueue/cr-1/pending && rm -f /tmp/z",
			queueBase: "queue",
			wantAllow: true,
		},
		{
			name:      "path-segment start after slash still denies",
			cmd:       "mv ../queue/cr-1/pending/x.md ../queue/cr-1/done/",
			queueBase: "queue",
			wantAllow: false,
		},
		{
			name:      "non-default base denies same-clause mv on that base",
			cmd:       "mv board/cr-1/pending/x.md board/cr-1/done/",
			queueBase: "board",
			wantAllow: false,
		},
		{
			name:      "non-default base not fooled by keyboard/ substring",
			cmd:       "mv keyboard/cr-1/pending/x.md keyboard/cr-1/done/",
			queueBase: "board",
			wantAllow: true,
		},
		{
			name:      "non-Bash / empty command",
			cmd:       "",
			queueBase: "queue",
			wantAllow: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := QueueGuard(Input{ToolInput: toolInput{Command: tc.cmd}}, tc.queueBase)
			if tc.wantAllow {
				if got != allow {
					t.Errorf("QueueGuard(%q) = %+v, want allow", tc.cmd, got)
				}
				return
			}
			if got.ExitCode != 2 {
				t.Errorf("QueueGuard(%q).ExitCode = %d, want 2 (deny)", tc.cmd, got.ExitCode)
			}
		})
	}
}

// TestQueueGuard_GuidanceNamesOnlyRealVerbs: the refusal is the text a blocked agent reads
// to find the sanctioned path, so it must name commands that exist. The legacy shell
// wrappers (queue-move.sh, send-to-review.sh, gate-verdict.sh, assign.sh) are not shipped.
func TestQueueGuard_GuidanceNamesOnlyRealVerbs(t *testing.T) {
	got := QueueGuard(Input{ToolInput: toolInput{Command: "mv queue/cr-1/pending/1.md queue/cr-1/done/1.md"}}, "queue")
	if got.ExitCode != 2 {
		t.Fatalf("setup: want a block, got %+v", got)
	}
	if strings.Contains(got.Stderr, ".sh") {
		t.Errorf("guidance names a shell wrapper that does not exist:\n%s", got.Stderr)
	}
	for _, verb := range []string{
		"leadkit queue move",
		"leadkit review send",
		"leadkit gate verdict",
		"leadkit queue assign",
		"leadkit queue sweep",
	} {
		if !strings.Contains(got.Stderr, verb) {
			t.Errorf("guidance does not name %q:\n%s", verb, got.Stderr)
		}
	}
}
