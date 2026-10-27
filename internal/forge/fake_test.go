package forge

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// reply is one canned forge answer. A zero Status with no Err is a 200.
type reply struct {
	Body   string
	Status int    // non-2xx makes the fake fail the way gh/glab do
	Msg    string // the forge's message, mirrored into the stderr "(HTTP n)" line
	Err    error  // overrides everything: the CLI itself failed
}

// fakeRunner answers by "METHOD path" and records every call. It mimics gh/glab's error
// shape: JSON error body on stdout, "gh: <msg> (HTTP <n>)" on stderr, non-nil exit error.
type fakeRunner struct {
	replies map[string]reply
	calls   []fakeCall
	// onCall, if set, runs after a call is recorded, letting a test change later answers
	// (e.g. a successful PUT flips what the next GET returns).
	onCall func(f *fakeRunner, c fakeCall)
}

type fakeCall struct {
	Argv  []string
	Stdin string
}

func newFake(r map[string]reply) *fakeRunner { return &fakeRunner{replies: r} }

// key finds the "METHOD path" a recorded argv addresses: --method's value and the final arg.
func (c fakeCall) key() string {
	method := "GET"
	for i, a := range c.Argv {
		if a == "--method" && i+1 < len(c.Argv) {
			method = c.Argv[i+1]
		}
	}
	return method + " " + c.Argv[len(c.Argv)-1]
}

func (c fakeCall) isWrite() bool { return !strings.HasPrefix(c.key(), "GET ") }

func (f *fakeRunner) writes() []fakeCall {
	var w []fakeCall
	for _, c := range f.calls {
		if c.isWrite() {
			w = append(w, c)
		}
	}
	return w
}

func (f *fakeRunner) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	c := fakeCall{Argv: argv, Stdin: string(stdin)}
	f.calls = append(f.calls, c)
	if f.onCall != nil {
		defer f.onCall(f, c)
	}
	rp, ok := f.replies[c.key()]
	if !ok {
		return nil, nil, fmt.Errorf("fakeRunner: no reply for %q", c.key())
	}
	if rp.Err != nil {
		return nil, nil, rp.Err
	}
	if rp.Status == 0 || (rp.Status >= 200 && rp.Status < 300) {
		return []byte(rp.Body), nil, nil
	}
	cli := argv[0]
	body := rp.Body
	if body == "" {
		body = fmt.Sprintf(`{"message":%q}`, rp.Msg)
	}
	return []byte(body), []byte(fmt.Sprintf("%s: %s (HTTP %d)\n", cli, rp.Msg, rp.Status)), errors.New("exit status 1")
}

var errNotInstalled = fmt.Errorf("exec: %q: %w", "gh", exec.ErrNotFound)

var (
	ghTarget = Target{Kind: KindGitHub, Host: "github.com", Repo: "acme/widgets"}
	glTarget = Target{Kind: KindGitLab, Host: "gitlab.com", Repo: "acme/grp/widgets"}
)

const (
	ghProtPath   = "GET repos/acme/widgets/branches/main/protection"
	ghRulesPath  = "GET repos/acme/widgets/rules/branches/main"
	ghRepoPath   = "GET repos/acme/widgets"
	glProjPath   = "GET projects/acme%2Fgrp%2Fwidgets"
	glListPath   = "GET projects/acme%2Fgrp%2Fwidgets/protected_branches?per_page=100"
	ghStrictJSON = `{
  "required_status_checks": {"strict": true, "contexts": ["ci/build"], "checks": [{"context":"ci/build","app_id":1}]},
  "enforce_admins": {"url":"x","enabled": true},
  "required_pull_request_reviews": {"required_approving_review_count": 0},
  "restrictions": null,
  "allow_force_pushes": {"enabled": false},
  "allow_deletions": {"enabled": false}
}`
)

// ghRichJSON is a branch protected far beyond leadkit's standard, short of it only in that
// admins are exempt. The shape is the GET /protection document.
const ghRichJSON = `{
  "url": "https://api.github.com/repos/acme/widgets/branches/main/protection",
  "required_status_checks": {"strict": true, "contexts": ["ci/build","ci/lint","ci/test"],
    "checks": [{"context":"ci/build","app_id":15368},{"context":"ci/lint","app_id":null},{"context":"ci/test","app_id":null}]},
  "enforce_admins": {"url":"x","enabled": false},
  "required_pull_request_reviews": {"url":"x","dismiss_stale_reviews": true, "require_code_owner_reviews": true,
    "require_last_push_approval": true, "required_approving_review_count": 2,
    "dismissal_restrictions": {"url":"x","users":[{"login":"alice"}],"teams":[{"slug":"leads"}],"apps":[]}},
  "restrictions": {"url":"x","users":[{"login":"bob"}],"teams":[{"slug":"core"},{"slug":"release"}],"apps":[]},
  "required_linear_history": {"enabled": true},
  "allow_force_pushes": {"enabled": false},
  "allow_deletions": {"enabled": false},
  "block_creations": {"enabled": false},
  "required_conversation_resolution": {"enabled": true},
  "lock_branch": {"enabled": false},
  "allow_fork_syncing": {"enabled": false},
  "required_signatures": {"url":"x","enabled": false}
}`
