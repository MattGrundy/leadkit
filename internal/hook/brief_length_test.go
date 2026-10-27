package hook

import "testing"

func TestBriefLength(t *testing.T) {
	longMsg := ""
	for i := 0; i < 1500; i++ {
		longMsg += "x"
	}
	shortMsg := "a short brief"

	tests := map[string]struct {
		in   Input
		want Result
	}{
		"ungoverned tool passes": {
			in:   Input{ToolName: "Bash", ToolInput: toolInput{Message: longMsg}},
			want: allow,
		},
		"SendMessage, no message field": {
			in:   Input{ToolName: "SendMessage"},
			want: allow,
		},
		"SendMessage, short message": {
			in:   Input{ToolName: "SendMessage", ToolInput: toolInput{Message: shortMsg}},
			want: allow,
		},
		"SendMessage, long message with LONG-BRIEF marker bypasses": {
			in:   Input{ToolName: "SendMessage", ToolInput: toolInput{Message: "[LONG-BRIEF: re-briefing after an agent died] " + longMsg}},
			want: allow,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := BriefLength(tt.in, 0); got != tt.want {
				t.Errorf("BriefLength(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}

	t.Run("SendMessage, long message without marker blocks", func(t *testing.T) {
		got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: longMsg}}, 0)
		if got.ExitCode != 2 {
			t.Errorf("ExitCode = %d, want 2", got.ExitCode)
		}
		if got.Stdout != "" {
			t.Errorf("Stdout = %q, want empty (this style blocks via stderr)", got.Stdout)
		}
		if got.Stderr == "" {
			t.Fatal("want a non-empty stderr guidance message")
		}
		wantSubstr := "BLOCKED: agent brief is 1500 chars, over the 1400 limit."
		if got.Stderr[:len(wantSubstr)] != wantSubstr {
			t.Errorf("Stderr first line = %q, want prefix %q", got.Stderr, wantSubstr)
		}
	})
}

func TestBriefLength_MaxCharsEnvOverride(t *testing.T) {
	t.Setenv("BRIEF_GUARD_MAX_CHARS", "10")
	got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: "this is over ten characters"}}, 0)
	if got.ExitCode != 2 {
		t.Fatalf("want a block under the lowered max, got %+v", got)
	}
}

func TestBriefLength_RuneCountNotByteCount(t *testing.T) {
	// A multi-byte-per-rune string that's short in RUNES but long in bytes must NOT
	// be blocked — this is the documented rune-count-vs-byte-count deliberate choice.
	t.Setenv("BRIEF_GUARD_MAX_CHARS", "5")
	msg := "héllo" // 5 runes, more than 5 bytes (é is 2 bytes in UTF-8)
	got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: msg}}, 0)
	if got != allow {
		t.Errorf("BriefLength(%q) = %+v, want allow (5 runes == max)", msg, got)
	}
}

func TestBriefLength_DefaultConfigStillBlocksAtExactlySameLimitAsBefore(t *testing.T) {
	// The no-op proof: default profile (BriefLengthMax == config.DefaultBriefLengthMax,
	// currently 1400) must behave EXACTLY as the pre-config-wiring hardcode did.
	longMsg := ""
	for i := 0; i < 1401; i++ {
		longMsg += "x"
	}
	shortMsg := ""
	for i := 0; i < 1400; i++ {
		shortMsg += "x"
	}

	if got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: shortMsg}}, DefaultBriefMaxChars); got != allow {
		t.Errorf("1400 chars with cfgMax=DefaultBriefMaxChars must still be allowed, got %+v", got)
	}
	got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: longMsg}}, DefaultBriefMaxChars)
	if got.ExitCode != 2 {
		t.Errorf("1401 chars with cfgMax=DefaultBriefMaxChars must still block, got %+v", got)
	}
}

func TestBriefLength_ProfileMaxCharsDrivesTheLimitWhenEnvUnset(t *testing.T) {
	t.Setenv("BRIEF_GUARD_MAX_CHARS", "")
	got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: "this is over ten characters"}}, 10)
	if got.ExitCode != 2 {
		t.Fatalf("want a block under the profile's lowered max (10), got %+v", got)
	}
}

func TestBriefLength_EnvVarStillWinsOverProfileValue(t *testing.T) {
	// The pre-existing escape hatch must still take priority over the new
	// profile-driven value — nothing about it regresses.
	t.Setenv("BRIEF_GUARD_MAX_CHARS", "2000")
	longMsg := ""
	for i := 0; i < 1500; i++ {
		longMsg += "x"
	}
	got := BriefLength(Input{ToolName: "SendMessage", ToolInput: toolInput{Message: longMsg}}, 100)
	if got != allow {
		t.Errorf("env var (2000) should win over the profile's cfgMax (100), got %+v", got)
	}
}
