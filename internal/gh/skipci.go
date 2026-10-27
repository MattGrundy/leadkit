package gh

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// CANONICAL SOURCE: the adopter's CI scripts/check-commit-skip-ci.sh (mirrored to
// ~/.claude). Keep this in sync by hand — see the package doc.

// skipCIDetectPattern is the detection regex: "[skip ci]" case-insensitive with
// one-or-more whitespace between "skip" and "ci".
var skipCIDetectPattern = regexp.MustCompile(`(?i)\[skip[[:space:]]+ci\]`)

// hasSkipCIMarker is the pure detector, split out so it's directly testable without
// gh.
func hasSkipCIMarker(message string) bool {
	return skipCIDetectPattern.MatchString(message)
}

// commitAPI mirrors the subset of `gh api repos/<repo>/commits/<sha>` HasSkipCI needs.
type commitAPI struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// HasSkipCI is check-commit-skip-ci.sh: reports whether sha's commit message in repo
// carries a "[skip ci]" marker. Fetches the full commit object (rather than
// `--jq '.commit.message'`) and reads the message field in Go, per the spec's
// either-way note — one less moving part to keep the func var contract simple.
func HasSkipCI(repo, sha string) (bool, error) {
	out, err := runGH("api", fmt.Sprintf("repos/%s/commits/%s", repo, sha))
	if err != nil {
		return false, fmt.Errorf("gh: api repos/%s/commits/%s: %w", repo, sha, err)
	}
	var c commitAPI
	if err := json.Unmarshal(out, &c); err != nil {
		return false, fmt.Errorf("gh: parse commit json: %w", err)
	}
	return hasSkipCIMarker(c.Commit.Message), nil
}

// FormatSkipCIMessage is check-commit-skip-ci.sh's warning for a detected marker,
// pulled out as a pure function so its exact wording is unit-testable.
func FormatSkipCIMessage(sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("check-skip-ci: commit %s carries [skip ci] — GitHub creates ZERO checks for this push/PR event, not just one workflow.", short)
}
