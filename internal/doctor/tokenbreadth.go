package doctor

import (
	"fmt"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/forge"
)

// forgeTokenBreadthCheck (ticket 219) compares what the forge CLI's token can WRITE to against
// the repositories the profile grants. The CLI has no hook, so the token is the one bound on
// which repository it acts on that holds however a command was spelled; a token that reaches
// more than the grant covers leaves "which repository" enforced by text alone.
//
// It reports three states and never blurs them (the pattern of forgeBranchProtectionCheck):
//
//	SCOPED       every repository the token can write to is covered by the grant      -> OK, Info
//	BROADER      the token can write to repositories the grant does not cover         -> Warn, naming them
//	CANNOT DETERMINE  the scopes or the repository list could not be read             -> Warn, saying why
//
// It reads two things from the forge, both GETs: the token's classic OAuth scopes (the
// X-OAuth-Scopes header of `gh api user`; fine-grained tokens send none) and the repositories
// the token can push to (`user/repos`). It never reads, prints or stores the token itself.
//
// What it does not claim: a token scoped to one repository still opens, edits and merges PRs
// there. This bounds which repositories, not what happens inside one; the review gate is what
// bounds that. GitLab tokens are not inspected (their breadth is per project or group), which
// is reported as CANNOT DETERMINE rather than skipped.
func forgeTokenBreadthCheck(prof *config.Profile, git Git) Check {
	const name = "forge-token-breadth"
	if git == nil {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked (no git seam given)"}
	}
	cannot := func(why string) Check {
		return Check{Name: name, OK: false, Severity: SeverityWarn,
			Detail: "CANNOT DETERMINE how broad the forge token is: " + why + " — treat the token as able to reach every repository its account can until verified"}
	}

	origin := git.RemoteURL("")
	if origin == "" {
		// Without a repository there is no forge host to ask, and asking github.com on whatever
		// credential the machine has would make this a live API call from every directory.
		return Check{Name: name, OK: true, Severity: SeverityInfo,
			Detail: "not checked: no origin remote resolved here, so there is no forge to ask; run `leadkit doctor` inside a repository whose forge the agents use"}
	}
	_, spec, _ := config.MatchAuthorityKey(origin, prof.Authority, false)
	t, err := forge.ResolveTarget(origin, spec)
	if err != nil {
		return cannot(err.Error())
	}
	host, kind := t.Host, t.Kind
	if kind != "github" {
		return cannot("this forge is " + string(kind) + ", whose tokens are scoped per project or group and are not inspected here")
	}

	hostArgs := []string{}
	if host != "" && host != "github.com" {
		hostArgs = []string{"--hostname", host}
	}
	out, errOut, err := forgeRunner.Run(append(append([]string{"gh", "api", "-i"}, hostArgs...), "user"), nil)
	if err != nil {
		return cannot("`gh api user` failed: " + firstLine(string(errOut), err.Error()))
	}
	scopes, classic := parseOAuthScopes(string(out))

	if classic && !hasRepoWriteScope(scopes) {
		return Check{Name: name, OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("SCOPED: the token's scopes (%s) include no repo scope, so it cannot write to any repository", orNone(scopes))}
	}

	args := append(append([]string{"gh", "api", "--paginate"}, hostArgs...),
		"user/repos?per_page=100&affiliation=owner,collaborator,organization_member",
		"--jq", ".[] | select(.permissions.push == true) | .full_name")
	repoOut, errOut, err := forgeRunner.Run(args, nil)
	if err != nil {
		return cannot("the repositories the token can write to could not be listed: " + firstLine(string(errOut), err.Error()))
	}
	var reach []string
	for _, l := range strings.Split(string(repoOut), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			reach = append(reach, l)
		}
	}
	sort.Strings(reach)

	granted := grantedRepoKeys(prof, origin)
	var excess []string
	for _, r := range reach {
		if !coveredByGrant(host, r, granted) {
			excess = append(excess, r)
		}
	}

	kindWord := "a fine-grained or app token"
	if classic {
		kindWord = "a classic token with scope `repo`/`public_repo` (" + strings.Join(scopes, ", ") + "), which is account-wide and cannot be narrowed"
	}
	if len(excess) == 0 {
		return Check{Name: name, OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("SCOPED: %s can write to %d repositor%s, all covered by the grant (%s). It bounds which repositories, not what is done inside them", kindWord, len(reach), plural(len(reach)), orNone(granted))}
	}
	shown := excess
	more := ""
	if len(shown) > 8 {
		shown, more = shown[:8], fmt.Sprintf(" and %d more", len(excess)-8)
	}
	return Check{Name: name, OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("BROADER THAN THE GRANT: %s can write to %d repositor%s the grant does not cover: %s%s. The grant covers: %s. Replace it with a fine-grained token limited to the granted repositories (docs/PORTABLE-OPERATING-KIT.md); until then `which repository` is enforced by the text guards alone",
			kindWord, len(excess), plural(len(excess)), strings.Join(shown, ", "), more, orNone(granted))}
}

// parseOAuthScopes reads the X-OAuth-Scopes header out of `gh api -i` output. classic is false
// when the header is absent, which is what a fine-grained or app token produces (a classic
// token with no scopes sends the header empty).
func parseOAuthScopes(resp string) (scopes []string, classic bool) {
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break // end of headers
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "x-oauth-scopes") {
			continue
		}
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
		return scopes, true
	}
	return nil, false
}

func hasRepoWriteScope(scopes []string) bool {
	for _, s := range scopes {
		if s == "repo" || s == "public_repo" {
			return true
		}
	}
	return false
}

// grantedRepoKeys is every repository the profile grants: the authority keys, plus the repo the
// session is standing in (the project's own repository is governed without a key).
func grantedRepoKeys(prof *config.Profile, origin string) []string {
	var keys []string
	for k := range prof.Authority {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if origin != "" {
		covered := false
		for _, k := range keys {
			covered = covered || config.OriginMatchesProject(origin, k)
		}
		if !covered {
			keys = append(keys, config.RedactOrigin(origin))
		}
	}
	sort.Strings(keys)
	return keys
}

// coveredByGrant reports whether the repository host/full is one a granted key names, at a
// path boundary (the rule that matches project_origin), never by substring. Case is folded: the
// forge reports a repository in its canonical case and its names are case-insensitive, so a key
// spelled `Acme/Widgets` does cover `acme/widgets` here (project_origin itself stays exact).
func coveredByGrant(host, full string, granted []string) bool {
	for _, k := range granted {
		if config.OriginMatchesProject(strings.ToLower(host+"/"+full), strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return fallback
	}
	return s
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
