package testrun

import (
	"fmt"
	"strings"
)

// FormatVerdict renders v as `test run`'s human-readable one-liner (or a few
// lines, for red): "tests: green @<sha12>" or "tests: RED @<sha12> — N failing:"
// followed by one indented line per failing test.
func FormatVerdict(v Verdict) string {
	sha12 := shortSHA(v.SHA)
	if v.Verdict == "green" {
		return fmt.Sprintf("tests: green @%s\n", sha12)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tests: RED @%s — %d failing:\n", sha12, len(v.Failing))
	for _, f := range v.Failing {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return b.String()
}
