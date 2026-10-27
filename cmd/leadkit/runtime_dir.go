package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/config"
)

// orphanRuntimeDir is the guard for a command that is about to write into a `.leadkit/`
// runtime directory (decisions, spikes, the goal, the milestone artifact): it returns an error
// when the write would CREATE that directory in a repo no profile governs, and nil otherwise.
//
// Why a write must not do that: a profile-less session resolves every `.leadkit/...` default
// against its working directory (Profile.Resolve), so the first `decision record` in a repo with
// no profile would leave a `.leadkit/` there, and the push gate reads that directory as "this is
// a leadkit project" while having no profile to decide it by (ticket 302). A repo left in that
// state is still pushable (the gate reports it as unconfigured), but nothing should put it there
// by side effect. A directory that is already there is not this guard's business, and nothing
// is stopped in a repo that has a profile.
func orphanRuntimeDir(prof *config.Profile, path string) error {
	if prof.ProjectRoot() != "" || path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if filepath.Base(dir) != leadkitProjectDir {
		return nil
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	return fmt.Errorf("no leadkit profile governs this directory, so writing %s would create a %s/ directory here, which the push gate reads as a leadkit project it has no profile for; create the profile first (`leadkit config init`, or `leadkit config init --external`), then run this again", path, leadkitProjectDir)
}
