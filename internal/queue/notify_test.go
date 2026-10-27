package queue

import (
	"strings"
	"testing"
)

// withFakeNotifier swaps notifyExec for the duration of a test — the injectable seam
// that guarantees no real process is ever spawned. It records the argv it was handed.
func withFakeNotifier(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := notifyExec
	notifyExec = func(args []string) error {
		got = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { notifyExec = prev })
	return &got
}

func findingsStatus() SweepStatus {
	return BuildSweepStatus(sampleResult(), "", fixedClock) // 2 stalled, 1 broken
}

func TestNotify_OffWhenCommandEmpty(t *testing.T) {
	got := withFakeNotifier(t)
	fired, err := Notify("", 1, findingsStatus())
	if err != nil {
		t.Fatal(err)
	}
	if fired {
		t.Error("an empty notify_command must never fire")
	}
	if len(*got) != 0 {
		t.Errorf("notifier was exec'd with %v despite empty command", *got)
	}
}

func TestNotify_BelowThresholdDoesNotFire(t *testing.T) {
	got := withFakeNotifier(t)
	// 3 findings total (2 stalled + 1 broken); threshold 4 is not crossed.
	fired, err := Notify("notify-send {msg}", 4, findingsStatus())
	if err != nil {
		t.Fatal(err)
	}
	if fired {
		t.Error("below-threshold findings must not fire the notifier")
	}
	if len(*got) != 0 {
		t.Errorf("notifier exec'd below threshold: %v", *got)
	}
}

func TestNotify_AtThresholdFiresWithSubstitutedPlaceholders(t *testing.T) {
	got := withFakeNotifier(t)
	fired, err := Notify("notify-send {total} {stalled} {broken} {msg}", 3, findingsStatus())
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("at/above threshold the notifier must fire")
	}
	if len(*got) != 5 {
		t.Fatalf("argv = %v, want 5 fields ({msg} stays one arg)", *got)
	}
	if (*got)[0] != "notify-send" || (*got)[1] != "3" || (*got)[2] != "2" || (*got)[3] != "1" {
		t.Errorf("placeholders not substituted: %v", *got)
	}
	if !strings.Contains((*got)[4], "stalled lane(s)") || !strings.Contains((*got)[4], "cr-1") {
		t.Errorf("{msg} must summarize the findings as a single arg: %q", (*got)[4])
	}
}

func TestNotify_ZeroThresholdNormalizesToOne(t *testing.T) {
	got := withFakeNotifier(t)
	// A clean sweep (0 findings) must NOT fire even with a misconfigured threshold 0.
	fired, _ := Notify("notify-send {msg}", 0, BuildSweepStatus(SweepResult{}, "", fixedClock))
	if fired {
		t.Error("threshold 0 must normalize to 1 — a clean sweep never notifies")
	}
	if len(*got) != 0 {
		t.Errorf("notifier fired on a clean sweep: %v", *got)
	}
}
