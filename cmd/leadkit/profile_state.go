package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/redact"
)

// profileState says whether leadkit could find a profile, and if not, where it looked. It exists
// to keep two facts apart that the built-in defaults make look the same: "no policy governs this"
// and "I could not find the policy" (ticket 197). With no profile found, scope and authority
// both read as "none", so a missing HOME read as a project's decision not to gate, and as a
// missing grant.
type profileState struct {
	Found bool
	// Refused is a profile that EXISTS and was not accepted (it will not parse, or the loader
	// declined it, e.g. a project_origin that does not match this repo). It is its own state:
	// built-in defaults govern in its place, so it reads exactly like a missing profile unless
	// it is told apart (the fail-open this file exists to close, arriving by a second route).
	Refused bool
	Path    string // the profile file, when Found or Refused
	Why     string // when not Found: what was searched, or why the profile was refused
}

// redactCredentials removes URL userinfo from text that may quote a remote's URL, so a refusal
// never copies an embedded token into a transcript.
func redactCredentials(s string) string { return redact.URLCredentials(s) }

// refusedProfile is the state for a profile at path that failed to load with err.
func refusedProfile(path string, err error) profileState {
	return profileState{Refused: true, Path: path, Why: redactCredentials(fmt.Sprintf("the profile %s was found but was not accepted: %v", path, err))}
}

// sessionProfileState is the profile the acting session resolves, the one loadProfileOrDefaults
// loads. A profile that is found but will not load is reported as such rather than as absent.
func sessionProfileState() profileState {
	_, path, err := config.Load()
	switch {
	case err != nil:
		return refusedProfile(path, err)
	case path == "":
		return profileState{Why: notFoundWhy(sessionCwd())}
	}
	return profileState{Found: true, Path: path}
}

// targetProfileState is the profile that governs a repo directory: its own in-repo leadkit.yaml
// or its external config, found the way the push gate finds them (never $LEADKIT_PROFILE, which
// names the session's profile and not the target's).
func targetProfileState(repo string) profileState {
	start, ok := resolveGoverningStart(repo, currentResolveBase())
	if !ok {
		return profileState{Why: fmt.Sprintf("%q is not a directory leadkit can resolve", repo)}
	}
	loc := config.ResolveLocations(start)
	path := loc.WalkUp
	if path == "" && loc.ExternalExists {
		path = loc.ExternalPath
	}
	if path == "" {
		return profileState{Why: notFoundWhy(start)}
	}
	// A file that is there is not yet a profile that was read: load it the way the gate does.
	if _, _, err := config.LoadFrom(start); err != nil {
		return refusedProfile(path, err)
	}
	return profileState{Found: true, Path: path}
}

// notFoundWhy names every place a profile is looked for from dir, and the HOME the external
// location was derived from: the facts an operator needs to see that it is the environment that
// is wrong and not the policy.
func notFoundWhy(dir string) string {
	loc := config.ResolveLocations(dir)
	var searched []string
	if env := os.Getenv("LEADKIT_PROFILE"); env != "" {
		searched = append(searched, "$LEADKIT_PROFILE="+env+" (not a usable file)")
	}
	searched = append(searched, "an in-repo leadkit.yaml at or above "+dir)
	if loc.ExternalPath != "" {
		searched = append(searched, "the external config "+loc.ExternalPath)
	}
	return fmt.Sprintf("no leadkit profile was found (looked for %s); HOME=%q", strings.Join(searched, ", "), os.Getenv("HOME"))
}

// profileMissingNote is the sentence a refusal gives when the cause is an unresolved profile and
// not a policy: "" when a profile resolves. It never suggests an authority grant, which is the
// owner's to give and not the cause here.
func profileMissingNote() string {
	if st := sessionProfileState(); !st.Found {
		if st.Refused {
			return st.Why + ". The profile exists but was not accepted; fix it or the repo's origin, do not add an authority entry"
		}
		return st.Why + ". This is an environment problem, not a missing grant: fix HOME/the working directory so the profile can be found; do not add an authority entry"
	}
	return ""
}

// repoGovernance is where a repo stands on "is there a policy that governs it", the question
// the push gate asks before it trusts an out-of-scope verdict (tickets 204, 302). The answers
// are kept apart because the built-in defaults make most of them read the same:
//
//	govNone          nothing says the repo is a leadkit project: the hook fires on every push on
//	                 the machine, so this is allowed silently.
//	govProfiled      a profile was read. Its scope decides.
//	govProblem       a profile exists and was turned away, or the environment cannot be trusted
//	                 to have found one (a .leadkit/ directory, no profile, and no sign that the
//	                 HOME searched is the one leadkit is configured under). Whether the repo is
//	                 gated is unknown, so it is REFUSED.
//	govUnconfigured  a .leadkit/ directory, no profile anywhere, and a HOME that is demonstrably
//	                 where leadkit keeps its external configs. There is no policy to be
//	                 unreadable: not a refusal, and not a silent allow either. It is reported on
//	                 every decision (ticket 302).
type repoGovernance int

const (
	govNone repoGovernance = iota
	govProfiled
	govProblem
	govUnconfigured
)

// leadkitProjectDir is the directory a leadkit project keeps its own files in. Its presence
// is the sign that a repo is a leadkit project even when no profile can be found for it.
const leadkitProjectDir = ".leadkit"

// classifyRepo says where repo stands, and the text that explains it (the profile that was
// turned away, what was searched, or the state's name).
//
// Before anything else is decided, a profile that exists must not read as absent because the
// lookup's one handle, the origin's name, was changed: a config in the config directory that names
// the repo by a remote's URL is a profile the lookup missed, and the repo is refused whether or not
// it has a .leadkit/ directory. For a repo with no .leadkit/ only a definite match refuses
// (config.ExternalConfigsNamingRepoByRemote), because this is asked on every push on the machine.
//
// A .leadkit/ directory with no profile is the one place the answer depends on the environment.
// The external config lives under HOME, and a HOME that differs from the one the profile was
// written under hides a profile that exists (`HOME=/elsewhere git push`). So the state is
// UNCONFIGURED only when all of these hold, and each closes a way a profile that EXISTS would
// read as absent:
//
//   - HOME is the home directory of the account running leadkit (homeIsTheAccountsOwn), not a
//     directory someone pointed it at;
//   - the external config directory exists under it, so leadkit is configured there;
//   - no file in it names this repo by any handle (config.ExternalConfigsNamingRepo): the
//     lookup is keyed on the origin's name, so renaming or removing the origin makes a profile
//     unfindable by it while the directory still holds it.
//
// Failing any one is a refusal. What stays open: a repo with every remote removed, a
// differently-named clone directory and a config that carries no project_origin shares no handle
// with its profile, and where os/user falls back to $HOME the shell's `~name` is the only source
// for the account home. No client-side check closes those; branch protection on the forge does.
func classifyRepo(repo string) (repoGovernance, string) {
	start, ok := resolveGoverningStart(repo, currentResolveBase())
	if !ok {
		return govNone, "" // not a directory the hook can resolve: the ordinary bad-repo path judges it
	}
	st := targetProfileState(repo)
	switch {
	case st.Found:
		return govProfiled, ""
	case st.Refused:
		return govProblem, st.Why
	}
	loc := config.ResolveLocations(start)
	searched := "an in-repo leadkit.yaml at or above " + start
	if loc.ExternalPath != "" {
		searched += ", the external config " + loc.ExternalPath
	}
	configDir := ""
	if loc.ExternalPath != "" && os.Getenv("HOME") != "" {
		configDir = filepath.Dir(loc.ExternalPath)
	}
	if fi, err := os.Stat(filepath.Join(start, leadkitProjectDir)); err != nil || !fi.IsDir() {
		// No sign of leadkit in the repo, which is not the same as no profile (see above). Only a
		// definite match refuses here, because this is asked on every push on the machine.
		if fi, err := os.Stat(configDir); configDir != "" && err == nil && fi.IsDir() {
			if named := config.ExternalConfigsNamingRepoByRemote(configDir, loc.RepoRoot); len(named) > 0 {
				return govProblem, fmt.Sprintf("%s: no profile was found by the lookup (looked for %s), but a config in %s names this repo by a remote, so a profile exists and was not found (the lookup is keyed on the origin's name; if a remote was renamed, `git remote rename <name> origin` restores it): %s",
					start, searched, configDir, strings.Join(named, "; "))
			}
		}
		return govNone, ""
	}
	notFound := fmt.Sprintf("%s has a %s directory, so it is a leadkit project, but no profile was found (looked for %s); ", start, leadkitProjectDir, searched)
	if !homeIsTheAccountsOwn() {
		return govProblem, notFound + fmt.Sprintf("HOME=%q is not the home directory of the account running leadkit, so a profile under the account's own home could not have been found", os.Getenv("HOME"))
	}
	if fi, err := os.Stat(configDir); configDir == "" || err != nil || !fi.IsDir() {
		return govProblem, notFound + fmt.Sprintf("HOME=%q holds no leadkit config directory, so a profile that exists under another HOME could not have been found", os.Getenv("HOME"))
	}
	if named := config.ExternalConfigsNamingRepo(configDir, loc.RepoRoot); len(named) > 0 {
		return govProblem, notFound + "but a config in " + configDir + " may belong to this repo by another handle than the lookup uses (a renamed or removed remote, or a clone directory named differently), so a profile may exist and not have been found: " + strings.Join(named, "; ")
	}
	return govUnconfigured, fmt.Sprintf("%s has a %s directory but no leadkit profile exists for it (looked for %s; HOME=%q)", start, leadkitProjectDir, searched, os.Getenv("HOME"))
}

// hookProfileProblem is push-gate's third state (ticket 204): why the profile that governs
// repo could not be READ, or "" when it was read, when nothing says the repo is a leadkit
// project at all, or when the repo is unconfigured (hookUnconfigured). The text names what was
// searched and the HOME the external location was derived from.
func hookProfileProblem(repo string) string {
	if g, why := classifyRepo(repo); g == govProblem {
		return why
	}
	return ""
}

// hookUnconfigured is push-gate's fourth state (ticket 302): the text of the unconfigured
// state for repo, or "" when the repo is in any other. It never overlaps hookProfileProblem.
func hookUnconfigured(repo string) string {
	if g, why := classifyRepo(repo); g == govUnconfigured {
		return why
	}
	return ""
}
