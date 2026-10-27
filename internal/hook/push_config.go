package hook

import (
	"os/exec"
	"strings"
)

// pushConfig is the git configuration a push is made under: the config files (read through
// gitConfigValues) overridden, last value wins, by what the push's own command line sets
// (gitPrefixConfig: `-c k=v`, `--config-env`, GIT_CONFIG_*). The command line is visible in the
// command text, so it is read; a value the hook cannot read (`--config-env`, a value built by an
// expansion) is the widening value, since it cannot be shown not to be.
type pushConfig struct {
	repo string
	cmd  []string // `key=value`, in the order git applies them
}

// canonKey folds a config key the way git does: the section and the variable name are
// case-insensitive, the subsection (the remote or branch name between them) is exact. So
// `remote.Origin.mirror` is a different setting from `remote.origin.mirror`, and an override of the
// first does not narrow the second.
func canonKey(k string) string {
	first, last := strings.Index(k, "."), strings.LastIndex(k, ".")
	if first < 0 {
		return strings.ToLower(k)
	}
	if first == last {
		return strings.ToLower(k)
	}
	return strings.ToLower(k[:first]) + k[first:last] + strings.ToLower(k[last:])
}

// unreadable reports whether a value cannot be known from the text.
func unreadable(v string) bool { return v == "?" || strings.ContainsAny(v, "$`") }

// last returns the effective value of key: the last command-line value, else the last file value.
func (c pushConfig) last(key string) (string, bool) {
	for i := len(c.cmd) - 1; i >= 0; i-- {
		if k, v, _ := strings.Cut(c.cmd[i], "="); canonKey(k) == canonKey(key) {
			return v, true
		}
	}
	if vs := gitConfigValues(c.repo, key); len(vs) > 0 {
		return vs[len(vs)-1], true
	}
	return "", false
}

// anyNonEmpty reports whether key is set to something in the files or on the command line.
func (c pushConfig) anyNonEmpty(key string) bool {
	for _, kv := range c.cmd {
		if k, v, _ := strings.Cut(kv, "="); canonKey(k) == canonKey(key) && v != "" {
			return true
		}
	}
	return len(gitConfigValues(c.repo, key)) > 0
}

// gitBool is what git itself reads key as, as a boolean, under the repo's config overridden by the
// command line's `-c` values (cmd, in order). set is false when the key is not set at all. Git's
// boolean is not a word list: true, yes, on, or ANY non-zero integer in any notation (decimal, hex,
// a k/m/g suffix) is true; false, no, off, zero and an empty value are false; a key with no `=` is
// true. A parser of that written here is wrong in the way every hand-written parser of it was (rounds
// of review found `=2`, `=42`, `=0x1` and a bare key all published and allowed), so git is asked, and
// asked with the same `-c` values the push will run under. A value git cannot read as a boolean makes
// git refuse to run the push, so it is not widening. A var so unit tests need no real repo.
var gitBool = func(repo string, cmd []string, key string) (value, set bool) {
	var args []string
	if repo != "" {
		args = append(args, "-C", repo)
	}
	for _, kv := range cmd {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG_") || strings.ContainsAny(kv, "$`") || v == "?" {
			continue // a channel or value the text does not hold: opaque() and unreadable() judge those
		}
		args = append(args, "-c", kv)
	}
	args = append(args, "config", "--type=bool", "--get", key)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return false, false // not set (exit 1), or not a boolean (git would refuse the push)
	}
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// boolean reports whether key is true for this push, as git reads it, or cannot be shown to be
// false: a command-line value built by an expansion is the widening value.
func (c pushConfig) boolean(key string) bool {
	for i := len(c.cmd) - 1; i >= 0; i-- {
		if k, v, _ := strings.Cut(c.cmd[i], "="); canonKey(k) == canonKey(key) {
			if unreadable(v) {
				return true
			}
			break
		}
	}
	v, set := gitBool(c.repo, c.cmd, key)
	return set && v
}

// opaque returns a reason if the command sets git configuration through a channel the hook cannot
// read at all (GIT_CONFIG_PARAMETERS, an unreadable GIT_CONFIG_COUNT).
func (c pushConfig) opaque() string {
	for _, kv := range c.cmd {
		k, _, _ := strings.Cut(kv, "=")
		if strings.ContainsAny(k, "$`") {
			// A key built by an expansion (`-c $K=matching`, `-c push.$D=matching`, `remote.$R.mirror`,
			// `GIT_CONFIG_KEY_0=$K`) can be any setting at all, including one that widens a push.
			return "the command sets git configuration under a key the hook cannot read (" + k + "), which could change what a push publishes"
		}
		if strings.EqualFold(k, "GIT_CONFIG_PARAMETERS") || strings.EqualFold(k, "GIT_CONFIG_COUNT") {
			return "the command sets git configuration the hook cannot read (" + k + "), which could change what a push publishes"
		}
	}
	return ""
}

// remote is the remote a push goes to. Named, it is the one named. A bare push uses, in git's
// order, branch.<current>.pushRemote, else remote.pushDefault, else branch.<current>.remote, else
// origin. Reading only branch.<b>.remote looked up the refspec on a remote git does not push to.
func (c pushConfig) remote(named string) string {
	if named != "" {
		return named
	}
	branch := ""
	if b, ok := runGit(c.repo, "rev-parse", "--abbrev-ref", "HEAD"); ok && b != "" && b != "HEAD" {
		branch = b
	}
	if branch != "" {
		if v, ok := c.last("branch." + branch + ".pushRemote"); ok && v != "" {
			return v
		}
	}
	if v, ok := c.last("remote.pushDefault"); ok && v != "" {
		return v
	}
	if branch != "" {
		if v, ok := c.last("branch." + branch + ".remote"); ok && v != "" {
			return v
		}
	}
	return "origin"
}

// mirrored says why a push to the remote publishes every ref whatever the command names:
// `remote.<name>.mirror = true` makes a push to it behave as `--mirror`, a named ref included.
func (c pushConfig) mirrored(remote string) string {
	if c.boolean("remote." + remote + ".mirror") {
		return "remote." + remote + ".mirror is set, so a push to " + remote + " publishes every ref, whatever the command names"
	}
	return ""
}

// unnamedRefSet says why a push that names no refspec would publish more than the current branch:
// `push.default=matching`, or a `remote.<name>.push` refspec on the remote it goes to, or that
// remote being a mirror. "" when it would not. Judging such a push by HEAD would be the fail-open
// this exists to close.
func (c pushConfig) unnamedRefSet(named string) string {
	if why := c.opaque(); why != "" {
		return why
	}
	if v, ok := c.last("push.default"); ok && (strings.EqualFold(v, "matching") || unreadable(v)) {
		return "push.default=matching publishes every branch that exists on both sides, not just the current one"
	}
	remote := c.remote(named)
	if why := c.mirrored(remote); why != "" {
		return why
	}
	key := "remote." + remote + ".push"
	if c.anyNonEmpty(key) {
		return key + " is configured, so a push that names no ref publishes what that refspec says, not just the current branch"
	}
	return ""
}

// unnamedRefSetConfig is unnamedRefSet for a push to `remote` ("" for a bare push) in repo under
// the command line's own configuration cmdConfig.
func unnamedRefSetConfig(repo, remote string, cmdConfig []string) string {
	return pushConfig{repo: repo, cmd: cmdConfig}.unnamedRefSet(remote)
}

// mirroredRemote says why a push to `remote` (named, or "" for a bare push) publishes every ref
// regardless of the refspecs it names, "" when it does not.
func mirroredRemote(repo, remote string, cmdConfig []string) string {
	c := pushConfig{repo: repo, cmd: cmdConfig}
	if why := c.opaque(); why != "" {
		return why
	}
	return c.mirrored(c.remote(remote))
}

// followTagsConfigured says why a push publishes tags it does not name although --follow-tags is not
// among its arguments: `push.followTags` is set (in the files or on the command line) and the push
// does not turn it off with --no-follow-tags. It is `--follow-tags` spelled as configuration, and is
// refused for the same reason. An abbreviation of --no-follow-tags is not read as the override:
// over-refusing a push that spells it short is cheaper than reading it wrongly. The boolean is git's
// own reading (pushConfig.boolean). Not read: a value that arrives through `-c include.path=<file>`
// or another include, which is the unbounded indirection of git configuration.
func followTagsConfigured(repo string, args, cmdConfig []string) string {
	if pushNegatesFollowTags(args) {
		return ""
	}
	if (pushConfig{repo: repo, cmd: cmdConfig}).boolean("push.followTags") {
		return "push.followTags is set, so every push also publishes the annotated tags it reaches"
	}
	return ""
}

// pushNegatesFollowTags reports whether a push's OPTIONS include `--no-follow-tags`. It walks the
// arguments the way parsePushArgs does, so a word that an option consumed as its VALUE is not read as an
// option: `-o --no-follow-tags` and `--push-option --no-follow-tags` hand the text to the server as a push
// option, and git, with push.followTags set, still publishes the tag.
func pushNegatesFollowTags(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		if a == "--no-follow-tags" {
			return true
		}
		if _, inline, isRepo := pushRepoOption(a); isRepo {
			if !inline && i+1 < len(args) {
				i++
			}
			continue
		}
		if pushOptionTakesNextWord(a) && i+1 < len(args) {
			i++
		}
	}
	return false
}
