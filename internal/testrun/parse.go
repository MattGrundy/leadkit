package testrun

import (
	"regexp"
	"strings"
)

// goFailRe matches Go's own `--- FAIL: TestName (0.00s)` lines.
var goFailRe = regexp.MustCompile(`^\s*--- FAIL:\s+(\S+)`)

// jestFailSuiteRe matches jest/vitest's `FAIL  path/to/file.test.ts` suite line.
var jestFailSuiteRe = regexp.MustCompile(`^FAIL\s+(\S+)`)

// jestFailCaseRe matches an individual failing test line — jest's "✕ name (12
// ms)" or vitest's "✗ name" / "× name".
var jestFailCaseRe = regexp.MustCompile(`^\s*[✕✗×]\s+(.+?)(?:\s+\(\d+\s*m?s\))?\s*$`)

// parseFailing scans a test runner's combined output for failing test names/
// paths — Go's `--- FAIL:` lines, and vitest/jest's failing-suite/failing-case
// lines. Unique, in first-seen order; unrecognized output yields an empty slice
// (never an error — the raw output is still available to whoever needs it).
func parseFailing(output string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}

	for _, line := range strings.Split(output, "\n") {
		if m := goFailRe.FindStringSubmatch(line); m != nil {
			add(m[1])
			continue
		}
		if m := jestFailSuiteRe.FindStringSubmatch(line); m != nil {
			add(m[1])
			continue
		}
		if m := jestFailCaseRe.FindStringSubmatch(line); m != nil {
			add(m[1])
			continue
		}
	}
	return out
}
