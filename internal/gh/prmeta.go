package gh

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// CANONICAL SOURCE: the shell merge wrapper this replaced, whose `gh pr view --json ...` metadata read and
// its `gh pr checks` green-gate. This package is a Go representation of the same reads —
// kept pure (a JSON parse split out from the fetch) so `pr merge`'s governance can be
// unit-tested against fixture JSON without ever shelling out to a real gh or GitHub.

// PRMeta mirrors the subset of `gh pr view <n> -R <repo> --json
// state,isDraft,mergeable,mergeStateStatus,headRefOid,title` that the merge wrapper read to
// gate a merge (the exact field set the legacy script fetches).
type PRMeta struct {
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	HeadRefOID       string `json:"headRefOid"`
	Title            string `json:"title"`
	BaseRefName      string `json:"baseRefName"`
	HeadRefName      string `json:"headRefName"`
	URL              string `json:"url"`
}

// parsePRMeta is the pure JSON-parse step, split out so it's directly testable against
// fixture JSON with no gh involved.
func parsePRMeta(data []byte) (PRMeta, error) {
	var m PRMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return PRMeta{}, fmt.Errorf("gh: parse pr view json: %w", err)
	}
	return m, nil
}

// FetchPRMeta reads a PR's merge-relevant metadata. A non-nil error means the state is
// UNREADABLE — the caller (pr merge) MUST fail closed (refuse), never merge on metadata
// it couldn't fetch.
func FetchPRMeta(repo, pr string) (PRMeta, error) {
	out, err := runGH("pr", "view", pr, "-R", repo, "--json",
		"state,isDraft,mergeable,mergeStateStatus,headRefOid,title,baseRefName,headRefName,url")
	if err != nil {
		return PRMeta{}, fmt.Errorf("gh: pr view %s -R %s: %w", pr, repo, err)
	}
	return parsePRMeta(out)
}

// CIChecksGreen reports whether `gh pr checks <n> -R <repo>` exits clean — the exact
// green-gate the merge wrapper used (`gh pr checks` exit 0 == all green). ANY non-zero exit
// (checks red, pending, or none reported) OR a gh-invocation failure returns green=false:
// for merge purposes every one of those is "not green", so the caller fails closed. The
// captured combined-ish output is returned for a diagnostic snippet; err is advisory
// (green already encodes the pass/fail decision).
func CIChecksGreen(repo, pr string) (green bool, output string, err error) {
	out, e := runGH("pr", "checks", pr, "-R", repo)
	if e != nil {
		return false, string(out), e
	}
	return true, string(out), nil
}

// mergeSkipCIPattern detects a CI-skip directive on a PR HEAD for the MERGE guard. It is
// deliberately BROADER than check-commit-skip-ci.sh's `[skip ci]`-only detector
// (skipci.go's skipCIDetectPattern): GitHub honours BOTH orderings — `[skip ci]` AND
// `[ci skip]` — to suppress the whole check-suite, so the irreversible merge gate must
// refuse on either. Kept separate from HasSkipCI so `leadkit skip-ci` stays a faithful
// mirror of the canonical bash while the merge HEAD check is strictly safer.
var mergeSkipCIPattern = regexp.MustCompile(`(?i)\[(?:skip[[:space:]]+ci|ci[[:space:]]+skip)\]`)

// hasMergeSkipCIMarker is the pure detector for the merge guard, split out so both
// orderings are directly testable without gh.
func hasMergeSkipCIMarker(message string) bool {
	return mergeSkipCIPattern.MatchString(message)
}

// mergeSkipCIStripPattern is the STRIP counterpart of mergeSkipCIDetect: it also
// consumes the leading whitespace before the bracket (like stripSkipCI does for the
// narrow `[skip ci]`) so a stripped run doesn't leave a double space, and it covers BOTH
// orderings for the merge squash body.
var mergeSkipCIStripPattern = regexp.MustCompile(`(?i)[[:space:]]*\[(?:skip[[:space:]]+ci|ci[[:space:]]+skip)\]`)

// stripMergeSkipCI removes every skip-ci directive (either ordering, any case) plus its
// leading whitespace — the merge squash body's strip.
func stripMergeSkipCI(s string) string {
	return mergeSkipCIStripPattern.ReplaceAllString(s, "")
}

// ComposeMerge composes the squash-merge subject/body for the MERGE path. It builds on
// the canonical Compose (which mirrors compose-squash-merge-message.sh and strips only
// the `[skip ci]` ordering) and then strips BOTH orderings — the squash body is
// safety-critical and GitHub honours `[ci skip]` too, so a merge must NEVER let either
// ride onto the merge commit (the #727/#728 guard, applied to either ordering). Kept
// separate from Compose so `leadkit merge-msg` stays a faithful bash mirror while the
// irreversible merge is strictly safer. A non-nil error means the compose could not be
// resolved; the caller (pr merge) MUST fail closed.
func ComposeMerge(repo, pr string) (subject, body string, err error) {
	subject, body, err = Compose(repo, pr)
	if err != nil {
		return "", "", err
	}
	return stripMergeSkipCI(subject), stripMergeSkipCI(body), nil
}

// HeadCarriesSkipCI reports whether sha's commit message in repo carries a CI-skip
// directive (`[skip ci]` or `[ci skip]`, any case, any internal-whitespace run) — the
// merge guard's HEAD check. A non-nil error means the commit was UNREADABLE; the caller
// (pr merge) MUST fail closed (refuse), never merge on a HEAD it couldn't read.
func HeadCarriesSkipCI(repo, sha string) (bool, error) {
	msg, err := CommitMessage(repo, sha)
	if err != nil {
		return false, err
	}
	return hasMergeSkipCIMarker(msg), nil
}

// CommitMessage reads the full message of the commit sha in repo. A non-nil error means it was
// UNREADABLE.
func CommitMessage(repo, sha string) (string, error) {
	out, err := runGH("api", fmt.Sprintf("repos/%s/commits/%s", repo, sha))
	if err != nil {
		return "", fmt.Errorf("gh: api repos/%s/commits/%s: %w", repo, sha, err)
	}
	var c commitAPI
	if err := json.Unmarshal(out, &c); err != nil {
		return "", fmt.Errorf("gh: parse commit json: %w", err)
	}
	return c.Commit.Message, nil
}

// MergedCommit is where a PR landed: its state and, once merged, the commit the base branch holds.
type MergedCommit struct {
	State string // MERGED once the forge has merged it; OPEN while a merge queue holds it
	OID   string // the merge (or squash) commit on the base branch; empty until merged
}

type mergedView struct {
	State       string `json:"state"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

// FetchMergeCommit reads which commit a PR was merged as. It is how `pr merge` learns the sha it
// actually landed, since the forge, not the CLI, creates a squash or merge commit.
func FetchMergeCommit(repo, pr string) (MergedCommit, error) {
	out, err := runGH("pr", "view", pr, "-R", repo, "--json", "state,mergeCommit")
	if err != nil {
		return MergedCommit{}, fmt.Errorf("gh: pr view %s -R %s: %w", pr, repo, err)
	}
	return parseMergedView(out)
}

func parseMergedView(data []byte) (MergedCommit, error) {
	var v mergedView
	if err := json.Unmarshal(data, &v); err != nil {
		return MergedCommit{}, fmt.Errorf("gh: parse pr view json: %w", err)
	}
	m := MergedCommit{State: v.State}
	if v.MergeCommit != nil {
		m.OID = v.MergeCommit.OID
	}
	return m, nil
}
