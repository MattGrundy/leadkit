package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/prcreate"
	"mgpp.dev/leadkit/internal/registry"
)

// openTerminal opens the process's controlling terminal. A var so a test can aim it elsewhere.
var openTerminal = func() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// confirmRetargetAtTerminal is the owner's per-use confirmation of a retarget. It runs at the moment
// of use, from `pr edit --base` itself, after every other check has passed: the owner reads what will
// change and types the PR number, and nothing is written to disk in between that anything could forge.
//
// What this stops: an agent whose shell has no controlling terminal cannot retarget. What it does not
// stop: a process that allocates its own pty (script, expect, python's pty module) and types the number
// itself. The trust boundary here is the human at the terminal, not the process; no check inside a
// process the agent can run can tell the two apart.
func confirmRetargetAtTerminal(f prcreate.RetargetFacts) error {
	tty, err := openTerminal()
	if err != nil {
		return errors.New("this process has no controlling terminal, and a retarget has to be confirmed by the owner at one. " +
			"Run the same command yourself in a terminal; an agent's shell has none")
	}
	defer tty.Close()
	return confirmTyped(tty, tty, f)
}

// confirmTyped shows the retarget and accepts only the PR number, typed in full, as the answer. Anything
// else, including end of input, declines.
func confirmTyped(in io.Reader, out io.Writer, f prcreate.RetargetFacts) error {
	fmt.Fprintf(out, "\nleadkit pr edit: RETARGET needs the owner's confirmation.\n  repo:  %s\n  PR:    #%s\n  base:  %s -> %s\n", f.Repo, f.PR, f.From, f.To)
	fmt.Fprintf(out, "Type the PR number (%s) to confirm, anything else cancels: ", f.PR)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return errors.New("no answer was typed")
	}
	if strings.TrimSpace(line) != f.PR {
		return fmt.Errorf("%q is not the PR number %s", strings.TrimSpace(line), f.PR)
	}
	return nil
}

// callerIsLiveLead reports whether THIS session is registered as a live lead. The role is written by
// the session itself (`leadkit register`) and the session id comes from the environment, so this is a
// bar and not a lock.
func callerIsLiveLead(prof *config.Profile) (bool, string) {
	host, sessionID, _ := registryIdentity(prof)
	if sessionID == "" {
		return false, "this session's id could not be resolved, so its role cannot be read"
	}
	entries, ok := registry.Open(resolveRegistryRepo(prof, "")).WithRemote(prof.RegistryRemote).ReadAll()
	if !ok {
		return false, "the identity registry could not be read"
	}
	return leadVerdict(entries, host, sessionID, time.Now().Add(-prof.RegistryTTLDur()))
}

// leadVerdict reads this session's own live entry: both its id AND its host must match, so another
// session's entry is never read as ours.
func leadVerdict(entries []registry.Entry, host, sessionID string, cutoff time.Time) (bool, string) {
	for _, e := range registry.Live(entries, cutoff) {
		if e.SessionID == sessionID && e.Host == host {
			if e.Role == registry.RoleLead {
				return true, fmt.Sprintf("%s is a live lead", e.DisplayName())
			}
			return false, fmt.Sprintf("this session is registered as %q, not lead", e.Role)
		}
	}
	return false, "this session is not registered as a live lead (leadkit register --role lead)"
}
