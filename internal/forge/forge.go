// Package forge is leadkit's Layer 0: forge-side branch protection. Every guard leadkit
// ships is a client-side hook, so a client that skips the hook (or never installed it)
// pushes straight past it. Only the forge rejecting the push server-side is bypass-proof.
// This package does two things against GitHub and GitLab, both through the authenticated
// `gh` / `glab` CLIs (never a raw HTTP client, never a token of leadkit's own):
//
//   - Inspect reads a branch's server-side protection and classifies it.
//   - Protect composes (and, when told to, applies) the write that makes it strict.
//
// The classification never rounds up. Verified means the forge affirmatively reported
// every property leadkit requires. Anything the forge did not say, or that leadkit could
// not read, is Unknown, never Verified.
//
// All I/O goes through the injected Runner, so tests never shell out to gh/glab.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/prcreate"
	"mgpp.dev/leadkit/internal/redact"
)

// Kind selects which CLI (gh or glab) and which API shape every call in this package uses.
// Anything that is not KindGitLab is addressed as GitHub, so a new forge must be added here,
// not inferred.
type Kind string

const (
	KindGitHub Kind = "github"
	KindGitLab Kind = "gitlab"
)

// Target identifies one repository on one forge host. Host is passed to gh/glab as
// --hostname, so a self-hosted forge is addressed explicitly instead of being inferred from
// the working directory; for GitLab, Repo is the full group/subgroup/project path.
type Target struct {
	Kind Kind
	Host string // e.g. github.com, gitlab.example.com
	Repo string // owner/name, or the full group/subgroup/project path
}

// ResolveTarget maps an origin URL to a Target using the SAME forge detection as
// `pr create`/`pr merge` (prcreate.ForgeName), so every forge-facing verb agrees on which
// forge a repo lives on. An error means the forge or repo path could not be determined.
func ResolveTarget(originURL string, spec config.AuthoritySpec) (Target, error) {
	name, err := prcreate.ForgeName(originURL, spec)
	if err != nil {
		return Target{}, err
	}
	host, repo := prcreate.ParseOrigin(originURL)
	if host == "" || repo == "" {
		return Target{}, fmt.Errorf("cannot parse host and repository path from origin %q", redact.Origin(originURL))
	}
	return Target{Kind: Kind(name), Host: host, Repo: repo}, nil
}

// Runner executes an argv with optional stdin and returns stdout, stderr and the exec
// error. The seam that keeps every test off the real gh/glab.
type Runner interface {
	Run(argv []string, stdin []byte) (stdout, stderr []byte, err error)
}

// ExecRunner is the real Runner: it execs the CLI under a timeout.
type ExecRunner struct {
	Timeout time.Duration // 0 means the default
}

// Run execs argv[0] directly (no shell) and returns stdout and stderr separately, so a
// caller can read the forge's own message from stderr when err is non-nil. It enforces the
// timeout and says so in the error; a hung gh would otherwise block forever.
func (e ExecRunner) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	d := e.Timeout
	if d <= 0 {
		d = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s timed out after %s", argv[0], d)
	}
	return out.Bytes(), errb.Bytes(), err
}

// response is one forge API call's outcome. Status is the HTTP status when the CLI
// reported one (200 stands in for any success); 0 means the call never produced one
// (CLI missing, not authenticated, network down, timeout) and Err says why.
type response struct {
	Status int
	Body   []byte
	Msg    string // the forge's own message, or the CLI's stderr
	Err    error
}

func (r response) ok() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

var httpStatusRe = regexp.MustCompile(`\(HTTP (\d{3})\)`)

// apiArgv composes the gh/glab api invocation. Both CLIs take --hostname, so a
// self-hosted forge is addressed explicitly rather than inferred from the cwd.
func apiArgv(t Target, method, path string, hasBody bool) []string {
	var argv []string
	switch t.Kind {
	case KindGitLab:
		argv = []string{"glab", "api", "--hostname", t.Host, "--method", method}
		if hasBody {
			argv = append(argv, "--input", "-", "--header", "Content-Type: application/json")
		}
	default:
		argv = []string{"gh", "api", "--hostname", t.Host, "--method", method}
		if hasBody {
			argv = append(argv, "--input", "-")
		}
	}
	return append(argv, path)
}

func call(r Runner, t Target, method, path string, body []byte) response {
	stdout, stderr, err := r.Run(apiArgv(t, method, path, body != nil), body)
	if err == nil {
		return response{Status: 200, Body: stdout}
	}
	resp := response{Body: stdout, Err: err}
	if errors.Is(err, exec.ErrNotFound) {
		resp.Err = fmt.Errorf("%s is not installed or not on PATH", cliName(t))
		resp.Msg = resp.Err.Error()
		return resp
	}
	msg := strings.TrimSpace(string(stderr))
	if m := httpStatusRe.FindStringSubmatch(msg); m != nil {
		resp.Status, _ = strconv.Atoi(m[1])
	}
	// gh and glab both print the forge's JSON error body on stdout; its "message" is
	// the most precise thing to classify on ("Branch not protected" vs "Branch not
	// found" are both a 404).
	var eb struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(stdout, &eb) == nil && eb.Message != "" {
		msg = eb.Message
	}
	if msg == "" {
		msg = err.Error()
	}
	resp.Msg = msg
	return resp
}

func cliName(t Target) string {
	if t.Kind == KindGitLab {
		return "glab"
	}
	return "gh"
}

func (t Target) repoPath() string { return url.PathEscape(t.Repo) }

// DefaultBranch asks the forge for the repo's default branch. It returns an error when the
// forge cannot be read or reports none; it never falls back to a guessed "main", because
// protecting the wrong branch would read as success.
func DefaultBranch(r Runner, t Target) (string, error) {
	path := "repos/" + t.Repo
	if t.Kind == KindGitLab {
		path = "projects/" + t.repoPath()
	}
	resp := call(r, t, "GET", path, nil)
	if !resp.ok() {
		return "", fmt.Errorf("cannot read %s %s: %s", t.Kind, t.Repo, describe(resp))
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(resp.Body, &repo); err != nil || repo.DefaultBranch == "" {
		return "", fmt.Errorf("the forge did not report a default branch for %s", t.Repo)
	}
	return repo.DefaultBranch, nil
}

// Branches resolves which branches to protect/verify: the authority entry's
// protected_branches when set, else the repo's default branch. Globs are refused — the
// GitHub branch-protection API takes exact names, and a pattern leadkit cannot verify
// must not be reported as covered.
func Branches(r Runner, t Target, spec config.AuthoritySpec) ([]string, error) {
	if len(spec.ProtectedBranches) > 0 {
		var out []string
		for _, b := range spec.ProtectedBranches {
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			if strings.ContainsAny(b, "*?[") {
				return nil, fmt.Errorf("protected_branches entry %q is a pattern; list exact branch names", b)
			}
			out = append(out, b)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	b, err := DefaultBranch(r, t)
	if err != nil {
		return nil, err
	}
	return []string{b}, nil
}

func describe(r response) string {
	switch {
	case r.Msg != "" && r.Status != 0:
		return fmt.Sprintf("HTTP %d: %s", r.Status, r.Msg)
	case r.Msg != "":
		return r.Msg
	case r.Err != nil:
		return r.Err.Error()
	}
	return "unknown error"
}
