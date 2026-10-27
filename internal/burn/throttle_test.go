package burn

import (
	"strings"
	"testing"
	"time"
)

var resumeAtFixture = time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)

func TestThrottle(t *testing.T) {
	tests := map[string]struct {
		locked     bool
		sessionPct float64
		wantAction string
	}{
		"not locked, under pause_pct — no change":      {locked: false, sessionPct: 97.9, wantAction: ""},
		"not locked, at pause_pct — LOCK":              {locked: false, sessionPct: 98.0, wantAction: "LOCK"},
		"not locked, over pause_pct — LOCK":            {locked: false, sessionPct: 99.5, wantAction: "LOCK"},
		"locked, above resume_pct — HELD":              {locked: true, sessionPct: 95.0, wantAction: "HELD"},
		"locked, exactly at resume_pct — HELD (not <)": {locked: true, sessionPct: 90.0, wantAction: "HELD"},
		"locked, just under resume_pct — RESUME":       {locked: true, sessionPct: 89.9, wantAction: "RESUME"},
		"locked, well under resume_pct — RESUME":       {locked: true, sessionPct: 10.0, wantAction: "RESUME"},
		"not locked, well under pause_pct — no change": {locked: false, sessionPct: 5.0, wantAction: ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := Throttle(tt.locked, tt.sessionPct, 98.0, 90.0, resumeAtFixture)
			if got.Action != tt.wantAction {
				t.Errorf("Throttle(locked=%v, %.1f%%) = %q, want %q", tt.locked, tt.sessionPct, got.Action, tt.wantAction)
			}
		})
	}
}

func TestThrottle_LockReasonMentionsResumeAndResetWindow(t *testing.T) {
	got := Throttle(false, 98.5, 98.0, 90.0, resumeAtFixture)
	if got.Action != "LOCK" {
		t.Fatalf("Action = %q, want LOCK", got.Action)
	}
	for _, want := range []string{"98.5%", "90%", resumeAtFixture.Format(time.RFC3339)} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", got.Reason, want)
		}
	}
}

func TestThrottle_HeldReasonMatchesSpecFormat(t *testing.T) {
	got := Throttle(true, 93.2, 98.0, 90.0, resumeAtFixture)
	if got.Action != "HELD" {
		t.Fatalf("Action = %q, want HELD", got.Action)
	}
	want := "HELD (locked, 93.2%, resumes <90%)"
	if got.Reason != want {
		t.Errorf("Reason = %q, want %q", got.Reason, want)
	}
}

func TestThrottle_ResumeCarriesNoReason(t *testing.T) {
	got := Throttle(true, 50.0, 98.0, 90.0, resumeAtFixture)
	if got.Action != "RESUME" {
		t.Fatalf("Action = %q, want RESUME", got.Action)
	}
}

func TestAccountThrottle_Hysteresis(t *testing.T) {
	tests := map[string]struct {
		locked     bool
		weekPct    float64
		wantAction string
	}{
		"not locked, under pause_pct — no change": {locked: false, weekPct: 97.9, wantAction: ""},
		"not locked, at pause_pct — LOCK":         {locked: false, weekPct: 98.0, wantAction: "LOCK"},
		"not locked, over pause_pct — LOCK":       {locked: false, weekPct: 99.5, wantAction: "LOCK"},
		"locked, above resume_pct — HELD":         {locked: true, weekPct: 95.0, wantAction: "HELD"},
		"locked, exactly at resume_pct — HELD":    {locked: true, weekPct: 90.0, wantAction: "HELD"},
		"locked, just under resume_pct — RESUME":  {locked: true, weekPct: 89.9, wantAction: "RESUME"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := AccountThrottle(tt.locked, tt.weekPct, 98.0, 90.0, resumeAtFixture)
			if got.Action != tt.wantAction {
				t.Errorf("AccountThrottle(locked=%v, %.1f%%) = %q, want %q", tt.locked, tt.weekPct, got.Action, tt.wantAction)
			}
		})
	}
}

func TestAccountThrottle_LockReasonNamesWeeklyUsage(t *testing.T) {
	got := AccountThrottle(false, 98.5, 98.0, 90.0, resumeAtFixture)
	if got.Action != "LOCK" {
		t.Fatalf("Action = %q, want LOCK", got.Action)
	}
	for _, want := range []string{"account burn throttle", "weekly usage 98.5%", ">= pause_pct 98%"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", got.Reason, want)
		}
	}
}
