package burn

import "testing"

// ShedOrderRank is the shared tier->rank mapping every caller outside this package
// sorts shed order by, so what matters is not the absolute numbers but that the
// ORDERING is strict and that an undeclared or junk priority never outranks a
// declared one. A session that could win a tie by supplying nothing, or by supplying
// "CRITICAL!!", would be able to dodge preemption by accident.
func TestShedOrderRank_StrictOrderAndSafeDefault(t *testing.T) {
	low := ShedOrderRank(string(PriorityLow))
	normal := ShedOrderRank(string(PriorityNormal))
	high := ShedOrderRank(string(PriorityHigh))
	crit := ShedOrderRank(string(PriorityCritical))

	if !(low < normal && normal < high && high < crit) {
		t.Fatalf("ranks must be strictly increasing low<normal<high<critical, got %d %d %d %d",
			low, normal, high, crit)
	}

	// Anything unrecognised resolves to normal: never critical (which would protect
	// itself from preemption), and never below low (which would make junk the first
	// thing shed, hiding a config error as a scheduling decision).
	for _, raw := range []string{"", "   ", "banana", "CRITICAL!!", "criticall", "urgent", "0"} {
		if got := ShedOrderRank(raw); got != normal {
			t.Errorf("ShedOrderRank(%q) = %d, want normal (%d)", raw, got, normal)
		}
	}
}

// The mapping must be case- and space-insensitive, because the value arrives from a
// hand-edited profile field and from an env override. "High" losing to " high " would
// be a silent scheduling change nobody could see in the config.
func TestShedOrderRank_CaseAndSpaceInsensitive(t *testing.T) {
	want := ShedOrderRank(string(PriorityHigh))
	for _, raw := range []string{"high", "HIGH", "High", "  high", "high  ", "\thigh\n"} {
		if got := ShedOrderRank(raw); got != want {
			t.Errorf("ShedOrderRank(%q) = %d, want %d", raw, got, want)
		}
	}
	if ShedOrderRank("high") == ShedOrderRank(string(PriorityNormal)) {
		t.Fatal("high must not collapse onto normal, or the test above proves nothing")
	}
}
