package install

// The git pre-push hook (ticket 171). Git runs it itself, after the shell has expanded,
// aliased and substituted, so it sees the commits actually being published whatever the
// push was spelled as. It is the enforcement layer for a RAW `git push`; `leadkit push`
// gates in-process and passes --no-verify, which is why it never reaches this hook.
//
// What it is not: it is per clone (a fresh clone has none until `leadkit init` runs in it),
// and `--no-verify`, `core.hooksPath` and deleting the file all still switch it off. Those
// are a short, enumerable list, which is the point: internal/hook refuses the first two in
// text and `leadkit doctor` reports the rest.
//
// Install is additive and never clobbers: a pre-push hook that is not ours is left alone.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PrePushMarker is the line that identifies a hook leadkit wrote. Without it a hook is
// foreign and is never overwritten.
const PrePushMarker = "# leadkit-managed pre-push hook (ticket 171)"

// PrePushState is where a clone's pre-push hook stands.
type PrePushState string

const (
	PrePushInstalled     PrePushState = "installed"      // ours, current, executable, and git will run it
	PrePushMissing       PrePushState = "missing"        // no pre-push hook at all
	PrePushStale         PrePushState = "stale"          // ours, but not what init would write now
	PrePushNotExecutable PrePushState = "not-executable" // ours; git skips a hook without the execute bit
	PrePushForeign       PrePushState = "foreign"        // a pre-push hook that is not ours
	PrePushRedirected    PrePushState = "redirected"     // core.hooksPath sends git elsewhere
	PrePushNoRepo        PrePushState = "no-repo"        // not inside a git work tree
)

// PrePushStatus is the result of InspectPrePush.
type PrePushStatus struct {
	State    PrePushState
	Repo     string // the work tree's top level
	HookPath string // where git looks for pre-push (honours core.hooksPath)
	Redirect string // the core.hooksPath value, when set
	Where    string // where that value is set (config scope and file), when known
	Binary   string // the leadkit binary an installed hook of ours runs
	Want     []byte // the script init would write
	Detail   string
}

// PrePushScript is the hook init writes. It hands git's stdin (one line per ref) to
// `leadkit gate check-push`, which runs gate.CheckPush. A missing binary REFUSES the push:
// a hook that silently opens when its binary moves is indistinguishable from one that passes.
func PrePushScript(binaryPath string) []byte {
	q := "'" + strings.ReplaceAll(binaryPath, "'", `'\''`) + "'"
	return []byte(`#!/bin/sh
` + PrePushMarker + `
# Written by ` + "`leadkit init --apply --confirm`" + `; delete this file to remove it.
LEADKIT=` + q + `
if [ ! -x "$LEADKIT" ]; then
  echo "leadkit pre-push: $LEADKIT is missing or not executable, so this push cannot be gated and is refused. Re-run 'leadkit init --apply --confirm', or delete $0." >&2
  exit 1
fi
exec "$LEADKIT" gate check-push "$@"
`)
}

// hookBinary reads the binary an installed hook of ours runs.
func hookBinary(content []byte) (string, bool) {
	for _, line := range strings.Split(string(content), "\n") {
		rest, ok := strings.CutPrefix(line, "LEADKIT='")
		if !ok || !strings.HasSuffix(rest, "'") {
			continue
		}
		return strings.ReplaceAll(strings.TrimSuffix(rest, "'"), `'\''`, "'"), true
	}
	return "", false
}

func gitOut(dir string, args ...string) (string, bool) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// InspectPrePush reports the pre-push hook of the git work tree containing dir, as it
// would be written for binaryPath. It reads only.
func InspectPrePush(dir, binaryPath string) PrePushStatus {
	top, ok := gitOut(dir, "rev-parse", "--show-toplevel")
	if !ok || top == "" {
		return PrePushStatus{State: PrePushNoRepo, Detail: dir + " is not inside a git work tree"}
	}
	st := PrePushStatus{Repo: top, Want: PrePushScript(binaryPath)}
	hooks, ok := gitOut(top, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if !ok || hooks == "" {
		st.State = PrePushNoRepo
		st.Detail = "could not ask git where the hooks of " + top + " live"
		return st
	}
	st.HookPath = filepath.Join(hooks, "pre-push")
	if v, ok := gitOut(top, "config", "--get", "core.hooksPath"); ok && v != "" {
		st.Redirect = v
		if o, ok := gitOut(top, "config", "--show-origin", "--get", "core.hooksPath"); ok {
			st.Where, _, _ = strings.Cut(o, "\t")
		}
	}

	content, err := os.ReadFile(st.HookPath)
	switch {
	case err == nil && bytes.Contains(content, []byte(PrePushMarker)):
		st.Binary, _ = hookBinary(content)
		fi, _ := os.Stat(st.HookPath)
		switch {
		case fi != nil && fi.Mode()&0o111 == 0:
			st.State, st.Detail = PrePushNotExecutable, st.HookPath+" is not executable, and git skips a hook that is not"
		case !bytes.Equal(content, st.Want):
			st.State, st.Detail = PrePushStale, st.HookPath+" is leadkit's but differs from what `leadkit init` writes now (it runs "+st.Binary+")"
		default:
			st.State, st.Detail = PrePushInstalled, st.HookPath+" runs "+st.Binary
		}
	case err == nil:
		st.State, st.Detail = PrePushForeign, st.HookPath+" exists and is not leadkit's"
	default:
		st.State, st.Detail = PrePushMissing, "no pre-push hook at "+st.HookPath
	}
	if st.Redirect != "" && st.State != PrePushInstalled {
		// Git is not reading the file above at all: core.hooksPath names another directory.
		st.State = PrePushRedirected
		st.Detail = "core.hooksPath is set to " + st.Redirect
		if st.Where != "" {
			st.Detail += " (" + st.Where + ")"
		}
		st.Detail += ", so git runs the hooks there and leadkit's pre-push gate is not wired for this clone"
	}
	return st
}

// ApplyPrePush writes the hook for a Missing, Stale or NotExecutable status and does nothing
// for any other (a foreign or redirected hook is never touched). It reports whether it wrote.
func ApplyPrePush(st PrePushStatus) (bool, error) {
	switch st.State {
	case PrePushMissing, PrePushStale, PrePushNotExecutable:
	default:
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(st.HookPath), 0o755); err != nil {
		return false, fmt.Errorf("pre-push hook: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.HookPath), ".pre-push-*")
	if err != nil {
		return false, fmt.Errorf("pre-push hook: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(st.Want); err != nil {
		tmp.Close()
		return false, fmt.Errorf("pre-push hook: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("pre-push hook: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return false, fmt.Errorf("pre-push hook: %w", err)
	}
	if err := os.Rename(tmp.Name(), st.HookPath); err != nil {
		return false, fmt.Errorf("pre-push hook: %w", err)
	}
	return true, nil
}

// FormatPrePush renders the status as `leadkit init` prints it.
func FormatPrePush(st PrePushStatus) string {
	switch st.State {
	case PrePushNoRepo:
		return "pre-push hook: not in a git work tree — skipped (it is per clone; run `leadkit init` inside the repo)\n"
	case PrePushInstalled:
		return "pre-push hook: " + st.Detail + " (already installed)\n"
	case PrePushMissing:
		return "pre-push hook: " + st.HookPath + " (will install)\n"
	case PrePushStale, PrePushNotExecutable:
		return "pre-push hook: " + st.Detail + " (will repair)\n"
	case PrePushForeign:
		return "pre-push hook: " + st.Detail + " — left alone; to gate raw pushes add `exec <leadkit> gate check-push \"$@\"` to it (or remove it and re-run init)\n"
	default:
		return "pre-push hook: " + st.Detail + " — NOT installed; leadkit does not write into a redirected hooks directory\n"
	}
}
