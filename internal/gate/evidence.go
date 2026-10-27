package gate

import (
	"fmt"
	"strings"
	"unicode"
)

// OneLine collapses every run of whitespace and control characters to a single space. Evidence is
// printed beside other lines of `gate show`, which a reader trusts; a value that could start a line
// of its own could forge one of them.
func OneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// noMutantPrefix opens a stored exemption, so a reader of the record can tell "none applies, and
// here is why" from a list of mutants without a schema.
const noMutantPrefix = "no mutant applies, because "

// ResolveMutationEvidence turns the two things a reviewer may pass into the text stored on the layer,
// or refuses. A ready verdict must carry either mutants (which were applied and the named test each
// failed) or an explicit reason none applies; silence is refused. A changes-needed verdict may carry
// either and needs neither, since a blocking finding must never be gated behind paperwork.
//
// It checks that something was SAID, never what: the content is free text, is not a gate input, and
// is as honest as the self-asserted name beside it.
func ResolveMutationEvidence(verdict, mutants, noMutant string) (string, error) {
	mutants, noMutant = OneLine(mutants), OneLine(noMutant)
	switch {
	case mutants != "" && noMutant != "":
		return "", fmt.Errorf("gate: REFUSED — --mutants and --no-mutant contradict each other: either mutants were applied or none applies. Pass one.")
	case mutants != "":
		return mutants, nil
	case noMutant != "":
		return noMutantPrefix + noMutant, nil
	case verdict == "ready":
		return "", fmt.Errorf("gate: REFUSED — a ready verdict must carry mutation evidence. Pass --mutants \"<each mutant applied and the test that failed for it>\", or --no-mutant \"<why no mutant applies, e.g. a comment-only diff>\". Mutants that fail to compile kill nothing, and run with -count=1 so the test cache cannot fake a kill.")
	}
	return "", nil
}
