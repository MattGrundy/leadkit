package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/audit"
)

var workishFixture = audit.WorkishConfig{DescriptionLengthThreshold: 280, Markers: []string{"##", "```", "acceptance", "DoD"}}

func longDescription() string {
	return strings.Repeat("x", 300)
}

func TestTaskMechanism(t *testing.T) {
	tests := map[string]struct {
		in         Input
		wantBlock  bool
		wantStderr bool
	}{
		"non-Task tool passes": {
			in: Input{ToolName: "Bash", ToolInput: toolInput{Description: longDescription()}},
		},
		"gate task, long description — allowed (sanctioned use)": {
			in: Input{ToolName: "TaskCreate", ToolInput: toolInput{
				Subject: "gate coordination", Description: longDescription(),
				Metadata: map[string]interface{}{"gate": true},
			}},
		},
		"short non-gate task — allowed": {
			in: Input{ToolName: "TaskCreate", ToolInput: toolInput{Subject: "s", Description: "fix the thing"}},
		},
		"long non-gate task — blocked": {
			in:        Input{ToolName: "TaskCreate", ToolInput: toolInput{Subject: "s", Description: longDescription()}},
			wantBlock: true,
		},
		"fenced-code non-gate task — blocked": {
			in: Input{ToolName: "TaskCreate", ToolInput: toolInput{
				Subject: "s", Description: "short but has\n```go\nfmt.Println()\n```\n",
			}},
			wantBlock: true,
		},
		"TaskUpdate with a workish description — blocked": {
			in:        Input{ToolName: "TaskUpdate", ToolInput: toolInput{Subject: "s", Description: longDescription()}},
			wantBlock: true,
		},
		"TaskUpdate with no description change — allowed": {
			in: Input{ToolName: "TaskUpdate", ToolInput: toolInput{Subject: "s", Description: ""}},
		},
		"override marker in description — allowed": {
			in: Input{ToolName: "TaskCreate", ToolInput: toolInput{
				Subject: "s", Description: "[SESSION-TASK-OK: recovery bridge] " + longDescription(),
			}},
		},
		"override marker in subject — allowed": {
			in: Input{ToolName: "TaskCreate", ToolInput: toolInput{
				Subject: "[SESSION-TASK-OK: recovery bridge] s", Description: longDescription(),
			}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := TaskMechanism(tt.in, workishFixture)
			if tt.wantBlock {
				if got.ExitCode != 2 {
					t.Errorf("ExitCode = %d, want 2", got.ExitCode)
				}
				if got.Stderr == "" {
					t.Error("want non-empty Stderr guidance on block")
				}
				if got.Stdout != "" {
					t.Errorf("Stdout = %q, want empty (this style blocks via stderr)", got.Stdout)
				}
			} else if got != allow {
				t.Errorf("got %+v, want allow", got)
			}
		})
	}
}

func TestTaskMechanism_MalformedJSONFailsOpenViaDispatch(t *testing.T) {
	res := Dispatch(NameTaskMechanism, []byte("not json"), Config{QueueBase: "queue", LogPath: "/tmp/x.jsonl", Workish: workishFixture})
	if res != allow {
		t.Errorf("Dispatch(task-mechanism, malformed json) = %+v, want allow", res)
	}
}

func TestTaskMechanism_BlockMessageMatchesSpecWording(t *testing.T) {
	got := TaskMechanism(Input{ToolName: "TaskCreate", ToolInput: toolInput{Description: longDescription()}}, workishFixture)
	for _, want := range []string{
		"BLOCKED: this session task reads as real work",
		"Create a disk task under tasks/",
		"[SESSION-TASK-OK: <reason>]",
	} {
		if !strings.Contains(got.Stderr, want) {
			t.Errorf("Stderr missing %q:\n%s", want, got.Stderr)
		}
	}
}

// TestTaskMechanism_SharesAuditsWorkishPredicate proves the guard and the audit
// session lens can never disagree: TaskMechanism's block decision must track
// audit.IsWorkish exactly, across configs, not a hardcoded/reimplemented threshold.
func TestTaskMechanism_SharesAuditsWorkishPredicate(t *testing.T) {
	descs := []string{"short", longDescription(), "has a ``` fence", "plain acceptance criteria text"}
	cfgs := []audit.WorkishConfig{
		{DescriptionLengthThreshold: 280, Markers: []string{"##", "```", "acceptance", "DoD"}},
		{DescriptionLengthThreshold: 10, Markers: []string{"```"}},
		{DescriptionLengthThreshold: 1000, Markers: nil},
	}
	for _, cfg := range cfgs {
		for _, desc := range descs {
			in := Input{ToolName: "TaskCreate", ToolInput: toolInput{Subject: "s", Description: desc}}
			gotBlocked := TaskMechanism(in, cfg).ExitCode == 2
			wantBlocked := audit.IsWorkish(desc, cfg)
			if gotBlocked != wantBlocked {
				t.Errorf("cfg=%+v desc=%q: TaskMechanism blocked=%v, audit.IsWorkish=%v (must match)", cfg, desc, gotBlocked, wantBlocked)
			}
		}
	}
}
