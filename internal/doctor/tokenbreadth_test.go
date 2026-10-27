package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// tokenFake answers the two reads the check makes. scopesResp is the raw `gh api -i user`
// output; repos is the `user/repos` listing, one name per line. A non-empty *Err makes that
// call fail the way gh does (message on stderr, non-nil exit error).
type tokenFake struct {
	scopesResp, repos   string
	scopesErr, reposErr string
	argvs               [][]string
}

func (f *tokenFake) Run(argv []string, _ []byte) ([]byte, []byte, error) {
	f.argvs = append(f.argvs, argv)
	for _, a := range argv {
		if strings.HasPrefix(a, "user/repos") {
			if f.reposErr != "" {
				return nil, []byte(f.reposErr + "\nsecond line"), errors.New("exit status 1")
			}
			return []byte(f.repos), nil, nil
		}
	}
	if f.scopesErr != "" {
		return nil, []byte(f.scopesErr), errors.New("exit status 1")
	}
	return []byte(f.scopesResp), nil, nil
}

func (f *tokenFake) reposCalled() bool {
	for _, argv := range f.argvs {
		for _, a := range argv {
			if strings.HasPrefix(a, "user/repos") {
				return true
			}
		}
	}
	return false
}

const homeOrigin = "git@github.com:home/base.git" // a repo the session stands in that no test lists as reachable

const (
	classicRepo  = "HTTP/2.0 200 OK\nX-Oauth-Scopes: gist, read:org, repo, workflow\nContent-Type: application/json\n\n{\"login\":\"acme\"}"
	classicNoRep = "HTTP/2.0 200 OK\nX-Oauth-Scopes: gist, read:org\n\n{}"
	classicNone  = "HTTP/2.0 200 OK\nX-Oauth-Scopes: \n\n{}"
	fineGrained  = "HTTP/2.0 200 OK\nContent-Type: application/json\n\n{\"login\":\"acme\"}"
)

func tokenCheck(t *testing.T, f *tokenFake, origin string, grants ...string) Check {
	t.Helper()
	withForge(t, f)
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{}}
	for _, g := range grants {
		prof.Authority[g] = config.AuthoritySpec{}
	}
	return forgeTokenBreadthCheck(prof, fakeGit{ok: true, remote: origin})
}

func TestForgeTokenBreadth_BroaderThanTheGrantNamesTheExcess(t *testing.T) {
	for name, resp := range map[string]string{"classic repo scope": classicRepo, "fine-grained (no scopes header)": fineGrained} {
		t.Run(name, func(t *testing.T) {
			f := &tokenFake{scopesResp: resp, repos: "acme/widgets\nacme/secret-infra\nother/thing\n"}
			c := tokenCheck(t, f, ghOrigin, "acme/widgets")
			if c.OK || c.Severity != SeverityWarn {
				t.Fatalf("a token that reaches repos the grant does not cover must warn: %+v", c)
			}
			for _, w := range []string{"BROADER THAN THE GRANT", "acme/secret-infra", "other/thing", "2 repositories", "The grant covers: acme/widgets"} {
				if !strings.Contains(c.Detail, w) {
					t.Errorf("detail lacks %q: %s", w, c.Detail)
				}
			}
			named, _, _ := strings.Cut(c.Detail, "The grant covers")
			if strings.Contains(named, "acme/widgets") {
				t.Errorf("a granted repository is named as excess: %s", c.Detail)
			}
		})
	}
	t.Run("classic wording says it cannot be narrowed", func(t *testing.T) {
		c := tokenCheck(t, &tokenFake{scopesResp: classicRepo, repos: "other/thing\n"}, ghOrigin, "acme/widgets")
		if !strings.Contains(c.Detail, "classic token") || !strings.Contains(c.Detail, "cannot be narrowed") {
			t.Errorf("%s", c.Detail)
		}
	})
}

func TestForgeTokenBreadth_ScopedAppropriatelyIsOKAndSaysWhatItDoesNotBound(t *testing.T) {
	f := &tokenFake{scopesResp: fineGrained, repos: "acme/widgets\nacme/docs\n"}
	c := tokenCheck(t, f, ghOrigin, "acme/docs")
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("every writable repo is covered (one by the grant, one by the repo the session stands in): %+v", c)
	}
	for _, w := range []string{"SCOPED", "2 repositories", "not what is done inside"} {
		if !strings.Contains(c.Detail, w) {
			t.Errorf("detail lacks %q: %s", w, c.Detail)
		}
	}
}

func TestForgeTokenBreadth_ACoverMustBeAtAPathBoundary(t *testing.T) {
	// The substring trap: a grant on acme/widgets must not cover a repo whose name merely contains it.
	f := &tokenFake{scopesResp: fineGrained, repos: "acme/widgets-fork\nevilacme/widgets\nme/acme/widgets\n"}
	c := tokenCheck(t, f, homeOrigin, "acme/widgets")
	for _, r := range []string{"acme/widgets-fork", "evilacme/widgets"} {
		if !strings.Contains(c.Detail, r) {
			t.Errorf("%s is covered by a substring match: %s", r, c.Detail)
		}
	}
	// Host-qualified keys cover the same repo.
	f2 := &tokenFake{scopesResp: fineGrained, repos: "acme/widgets\n"}
	if c := tokenCheck(t, f2, homeOrigin, "github.com/acme/widgets"); !c.OK {
		t.Errorf("a host-qualified key must cover its repo: %+v", c)
	}
}

func TestForgeTokenBreadth_NoAuthorityEntryMeansOnlyTheHomeRepoIsGranted(t *testing.T) {
	c := tokenCheck(t, &tokenFake{scopesResp: fineGrained, repos: "acme/a\nacme/b\n"}, homeOrigin)
	if c.OK || !strings.Contains(c.Detail, "The grant covers: github.com/home/base") || !strings.Contains(c.Detail, "acme/a, acme/b") {
		t.Errorf("%+v", c)
	}
}

func TestForgeTokenBreadth_ClassicTokenWithoutARepoScopeCannotWriteAnywhere(t *testing.T) {
	for name, resp := range map[string]string{"gist and org": classicNoRep, "empty scope list": classicNone} {
		t.Run(name, func(t *testing.T) {
			f := &tokenFake{scopesResp: resp, repos: "other/thing\n"}
			c := tokenCheck(t, f, ghOrigin, "acme/widgets")
			if !c.OK || !strings.Contains(c.Detail, "no repo scope") {
				t.Fatalf("%+v", c)
			}
			if f.reposCalled() {
				t.Error("the repository list is irrelevant to a token with no repo scope and must not be read")
			}
		})
	}
}

func TestForgeTokenBreadth_PublicRepoScopeCanWriteToRepositories(t *testing.T) {
	resp := "HTTP/2.0 200 OK\nX-Oauth-Scopes: public_repo\n\n{}"
	f := &tokenFake{scopesResp: resp, repos: "other/thing\n"}
	c := tokenCheck(t, f, ghOrigin, "acme/widgets")
	if c.OK || !f.reposCalled() || !strings.Contains(c.Detail, "other/thing") {
		t.Errorf("public_repo writes to public repositories, so the list must be read and compared: %+v", c)
	}
}

func TestForgeTokenBreadth_CannotDetermineIsAWarningNeverSilence(t *testing.T) {
	cases := map[string]struct {
		f      *tokenFake
		origin string
		want   []string
	}{
		"gh not authenticated": {&tokenFake{scopesErr: "gh: To get started with GitHub CLI, please run: gh auth login"}, ghOrigin,
			[]string{"CANNOT DETERMINE", "gh auth login", "treat the token as able to reach every repository"}},
		"the repository list fails": {&tokenFake{scopesResp: classicRepo, reposErr: "gh: HTTP 403 (rate limited)"}, ghOrigin,
			[]string{"CANNOT DETERMINE", "could not be listed", "HTTP 403"}},
		"an origin whose forge cannot be told": {&tokenFake{}, "https://git.corp.example/acme/widgets.git", []string{"CANNOT DETERMINE", "cannot determine forge"}},
		"a gitlab origin":                      {&tokenFake{}, "https://gitlab.com/acme/widgets.git", []string{"CANNOT DETERMINE", "gitlab"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := tokenCheck(t, c.f, c.origin, "acme/widgets")
			if got.OK || got.Severity != SeverityWarn {
				t.Fatalf("an unreadable breadth must warn: %+v", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got.Detail, w) {
					t.Errorf("detail lacks %q: %s", w, got.Detail)
				}
			}
			if strings.Contains(got.Detail, "second line") {
				t.Errorf("only the first line of gh's stderr belongs in the report: %s", got.Detail)
			}
		})
	}
}

func TestForgeTokenBreadth_HostAndSeamAndTruncation(t *testing.T) {
	t.Run("a GitHub Enterprise host is passed to gh", func(t *testing.T) {
		f := &tokenFake{scopesResp: fineGrained, repos: ""}
		tokenCheck(t, f, "git@github.corp.example:acme/widgets.git", "acme/widgets")
		for _, argv := range f.argvs {
			if strings.Join(argv, " ") == "" || !strings.Contains(strings.Join(argv, " "), "--hostname github.corp.example") {
				t.Errorf("call without the host: %v", argv)
			}
		}
	})
	t.Run("no git seam", func(t *testing.T) {
		if c := forgeTokenBreadthCheck(&config.Profile{}, nil); !c.OK || !strings.Contains(c.Detail, "not checked") {
			t.Errorf("%+v", c)
		}
	})
	t.Run("a long excess list is truncated and counted", func(t *testing.T) {
		var repos []string
		for i := 0; i < 12; i++ {
			repos = append(repos, fmt.Sprintf("acme/r%02d", i))
		}
		c := tokenCheck(t, &tokenFake{scopesResp: fineGrained, repos: strings.Join(repos, "\n")}, homeOrigin, "x/y")
		if !strings.Contains(c.Detail, "12 repositories") || !strings.Contains(c.Detail, "and 4 more") || strings.Contains(c.Detail, "acme/r11") {
			t.Errorf("%s", c.Detail)
		}
	})
}

func TestParseOAuthScopes(t *testing.T) {
	for _, c := range []struct {
		in      string
		scopes  []string
		classic bool
	}{
		{classicRepo, []string{"gist", "read:org", "repo", "workflow"}, true},
		{classicNone, nil, true},
		{fineGrained, nil, false},
		{"X-OAuth-Scopes: repo\r\n\r\n{\"x-oauth-scopes\":\"admin\"}", []string{"repo"}, true},
		{"HTTP/2 200\n\nX-Oauth-Scopes: repo", nil, false}, // a header-looking line in the BODY is not a header
	} {
		got, classic := parseOAuthScopes(c.in)
		if classic != c.classic || strings.Join(got, ",") != strings.Join(c.scopes, ",") {
			t.Errorf("parseOAuthScopes(%q) = %v, %v; want %v, %v", c.in, got, classic, c.scopes, c.classic)
		}
	}
}

func TestForgeTokenBreadth_NoOriginAsksTheForgeNothing(t *testing.T) {
	f := &tokenFake{scopesResp: classicRepo, repos: "other/thing\n"}
	c := tokenCheck(t, f, "", "acme/widgets")
	if !c.OK || !strings.Contains(c.Detail, "not checked") {
		t.Errorf("%+v", c)
	}
	if len(f.argvs) != 0 {
		t.Errorf("with no repository there is no host to ask, and the check must not call the forge on whatever credential the machine has: %v", f.argvs)
	}
}

func TestForgeTokenBreadth_ACaseDifferenceInTheKeyDoesNotMakeAGrantedRepoExcess(t *testing.T) {
	// The forge returns canonical case and its names are case-insensitive.
	c := tokenCheck(t, &tokenFake{scopesResp: fineGrained, repos: "acme/widgets\n"}, homeOrigin, "Acme/Widgets")
	if !c.OK {
		t.Errorf("a key spelled Acme/Widgets covers acme/widgets: %+v", c)
	}
}

func TestForgeTokenBreadth_ReadsEveryPageOfWritableRepositoriesOnly(t *testing.T) {
	// Two properties of the listing that the fake cannot show by its answer: it must follow every
	// page (an account with more than one page of repositories otherwise reports SCOPED while the
	// excess sits on page two), and it must keep only repositories the token can PUSH to (that
	// filter is what "can write to" means).
	f := &tokenFake{scopesResp: fineGrained, repos: "acme/widgets\n"}
	tokenCheck(t, f, ghOrigin, "acme/widgets")
	var listing []string
	for _, argv := range f.argvs {
		for _, a := range argv {
			if strings.HasPrefix(a, "user/repos") {
				listing = argv
			}
		}
	}
	joined := strings.Join(listing, " ")
	for _, want := range []string{"--paginate", ".permissions.push == true", "--jq"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the repository listing lacks %q: %v", want, listing)
		}
	}
	scopes := strings.Join(f.argvs[0], " ")
	if !strings.Contains(scopes, " -i ") && !strings.HasSuffix(scopes, " -i user") && !strings.Contains(scopes, "api -i") {
		t.Errorf("the scopes call must ask for the response headers (-i): %v", f.argvs[0])
	}
}

func TestRun_IncludesTheForgeTokenBreadthCheck(t *testing.T) {
	// The check exists to make a broad token visible; a check that is not in the report at all is
	// the limiting case of a silent one.
	withForge(t, &tokenFake{scopesResp: classicRepo, repos: "acme/widgets\nother/thing\n"})
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{"acme/widgets": {}}
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", fakeGit{ok: true, remote: ghOrigin}, "", "", "", "", "", "", "", "")
	var got *Check
	for i := range report.Checks {
		if report.Checks[i].Name == "forge-token-breadth" {
			got = &report.Checks[i]
		}
	}
	if got == nil {
		t.Fatal("doctor.Run does not include forge-token-breadth")
	}
	if got.OK || got.Severity != SeverityWarn || !strings.Contains(got.Detail, "other/thing") {
		t.Errorf("a token broader than the grant must warn through Run, got %+v", *got)
	}
}
