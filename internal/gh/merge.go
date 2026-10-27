package gh

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CANONICAL SOURCE: the adopter's CI scripts/compose-squash-merge-message.sh (mirrored
// to ~/.claude). Keep this in sync by hand — see the package doc.

// skipCIPattern matches compose-squash-merge-message.sh's strip regex: optional
// leading whitespace, then "[skip ci]" (case-insensitive, one-or-more spaces between
// "skip" and "ci").
var skipCIPattern = regexp.MustCompile(`[[:space:]]*\[[Ss][Kk][Ii][Pp][[:space:]]+[Cc][Ii]\]`)

// stripSkipCI removes every "[skip ci]" marker (any case, any run of internal
// whitespace), plus its leading whitespace, from s.
func stripSkipCI(s string) string {
	return skipCIPattern.ReplaceAllString(s, "")
}

// prView mirrors the subset of `gh pr view --json title,number,commits` Compose needs.
type prView struct {
	Title   string `json:"title"`
	Number  int    `json:"number"`
	Commits []struct {
		MessageHeadline string `json:"messageHeadline"`
		MessageBody     string `json:"messageBody"`
	} `json:"commits"`
}

// parsePRView is the pure JSON-parse step, split out so it's directly testable
// against fixture JSON.
func parsePRView(data []byte) (prView, error) {
	var v prView
	if err := json.Unmarshal(data, &v); err != nil {
		return prView{}, fmt.Errorf("gh: parse pr view json: %w", err)
	}
	return v, nil
}

// composeFromView is Compose's pure logic, split out from the gh fetch so it's
// directly testable against a parsed fixture, no gh involved.
func composeFromView(v prView) (subject, body string) {
	subject = stripSkipCI(fmt.Sprintf("%s (#%d)", v.Title, v.Number))

	var parts []string
	for _, c := range v.Commits {
		part := "* " + c.MessageHeadline
		if c.MessageBody != "" {
			part += "\n\n" + c.MessageBody
		}
		parts = append(parts, part)
	}
	body = stripSkipCI(strings.Join(parts, "\n\n"))
	return subject, body
}

// Compose is compose-squash-merge-message.sh: builds the squash-merge commit
// subject/body for pr in repo from `gh pr view`'s title/number/commits, stripping any
// "[skip ci]" marker from both (a squash-merge commit must never carry one — GitHub
// would then skip CI on the merge push itself).
func Compose(repo, pr string) (subject, body string, err error) {
	out, err := runGH("pr", "view", pr, "-R", repo, "--json", "title,number,commits")
	if err != nil {
		return "", "", fmt.Errorf("gh: pr view %s -R %s: %w", pr, repo, err)
	}
	v, err := parsePRView(out)
	if err != nil {
		return "", "", err
	}
	subject, body = composeFromView(v)
	return subject, body, nil
}

// FormatMergeMessage formats subject/body into the exact "<subject>\n---BODY---\n<body>\n"
// wire format the merge wrapper parses on the ---BODY--- delimiter. Pulled out as a pure
// function so the byte-exact format is unit-testable without capturing stdout.
func FormatMergeMessage(subject, body string) string {
	return subject + "\n---BODY---\n" + body + "\n"
}
