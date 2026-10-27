package hook

// GuardMode is a security guard's enforcement posture, resolved per-target and injected
// into the guard by cmd/leadkit (from the TARGET's governing profile — see
// config.Profile.EffectiveGuardMode). It is defined HERE, not imported from
// internal/config, so the pure guards (push-guard, push-gate, owner-guard) keep their
// deliberate config-free shape — cmd/leadkit translates the config string into this type.
//
// The distinction is ONLY on the UNVERIFIABLE path:
//   - ModeAdvisory (the DEFAULT, today's behavior): a guard that cannot verify safety
//     (an error / unparseable / unresolvable-target / unreadable-policy condition) fails
//     OPEN — allow. A clear positive violation still denies; a clear positive allow still
//     allows. This is byte-identical to the pre-enforcement behavior.
//   - ModeEnforce: the SAME unverifiable condition fails CLOSED — deny, authoritative. A
//     clear positive allow still allows and a clear violation still denies, exactly as in
//     advisory; only the ambiguous/unverifiable case flips.
type GuardMode int

const (
	// ModeAdvisory is the zero value on purpose: a guard whose config wires no mode
	// resolver (every existing test, every unconfigured adopter) is advisory — fail-open,
	// unchanged.
	ModeAdvisory GuardMode = iota
	ModeEnforce
)

// String renders the mode for a guard's deny reason (so logs/doctor show whether a deny
// was advisory or enforcing).
func (m GuardMode) String() string {
	if m == ModeEnforce {
		return "enforce"
	}
	return "advisory"
}

// Enforcing reports whether the mode is fail-CLOSED.
func (m GuardMode) Enforcing() bool { return m == ModeEnforce }
