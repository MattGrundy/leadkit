package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/config"
)

// fileProfileProblem is why the profile that governs a FILE target is not simply the profile in
// force, for the guards that resolve a profile per edited file (owner-guard, task-status-guard).
// A profile that EXISTS and did not load must not be replaced by the built-in defaults: they have
// no owners_file, so a one-line YAML error would switch the project's boundary off in the mode
// meant to fail closed.
type fileProfileProblem struct {
	// Mismatch is a config that names a different origin than this clone. It is not a failure to
	// read the policy but a determinate answer, the one a fork gives (ticket 136): the profile does
	// not govern this repo, there is no boundary, and the guards allow.
	Mismatch bool
	// Unreadable is a profile that exists and did not load, read leniently where it could be.
	Unreadable bool
	Path       string // the profile file
	Why        string // for the deny and the note: the profile and the error
}

// governingFileProfile resolves the profile governing targetPath and says what, if anything, is
// wrong with it. The returned profile is always safe to act on:
//
//   - loaded: the profile itself;
//   - none found: the built-in defaults (the unprofiled case, unchanged);
//   - another repo's (Mismatch): the built-in defaults, with the problem set for the note;
//   - found and did not load: a stand-in from the lenient read, fail-closed (config.Lenient), or the
//     strictest profile when that read fails too.
func governingFileProfile(targetPath string, b hookResolveBase) (*config.Profile, *fileProfileProblem) {
	start := normalizeFileTarget(targetPath, b)
	prof, found, err := config.LoadFrom(start)
	switch {
	case err == nil && found && prof != nil:
		return prof, nil
	case err == nil:
		d := config.Defaults()
		return &d, nil
	}
	path, root := failedProfileFile(start)
	if config.IsOriginMismatch(err) {
		d := config.Defaults()
		return &d, &fileProfileProblem{Mismatch: true, Path: path, Why: "the profile " + path + " names a different origin than this clone (" + err.Error() + "), so it does not govern this repo and no owners boundary applies here"}
	}
	prob := &fileProfileProblem{Unreadable: true, Path: path, Why: redactCredentials(fmt.Sprintf("the profile %s was found but was not accepted: %v", path, err))}
	stand := config.UnreadableStandIn(root)
	if path != "" {
		// A file that loads on its own is not the cause: the error is in a layer the lenient read does
		// not see, so nothing here can be vouched for.
		if _, ok, perr := config.LoadProjectFile(path); !(ok && perr == nil) {
			if data, rerr := os.ReadFile(path); rerr == nil {
				if l, ok := config.ReadLenient(data); ok {
					stand = l.StandIn(root)
				}
			}
		}
	}
	// The grant keys are the OPERATOR's when an external config exists for the repo, which is the
	// reason a committed file cannot disarm them: a committed profile that fails to load must not
	// either. What the committed file said (or the strictest reading, when it said nothing readable)
	// stands only where there is no operator config.
	if applied, aerr := config.ApplyOperatorGrants(&stand, start); aerr != nil {
		stand = config.UnreadableStandIn(root) // the operator's own config is unreadable too
		prob.Why += "; " + redactCredentials(aerr.Error())
	} else if applied {
		prob.Why += ", and the operator's external config governs the grant keys"
	}
	return &stand, prob
}

// failedProfileFile is the profile file LoadFrom tried for start (the in-repo file found by walking
// up, else the external config) and the root its relative paths anchor to.
func failedProfileFile(start string) (path, root string) {
	loc := config.ResolveLocations(start)
	if loc.WalkUp != "" {
		return loc.WalkUp, filepath.Dir(loc.WalkUp)
	}
	if loc.ExternalExists {
		return loc.ExternalPath, loc.RepoRoot
	}
	return "", start
}
