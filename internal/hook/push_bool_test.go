package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Review of 163 and 229 found the same defect in both: git's boolean was parsed by a four-word list,
// so `remote.up.mirror=2`, `=42`, `=-1`, `=0x1`, `=1k` and a key with no value at all made git publish
// every ref while the guard saw an ordinary push. A boolean is whatever git says it is, so the
// ground truth here is git: each spelling is written to a real repo, `git push up` is run for real,
// and the guard's reading is compared with whether the branch that a mirror push adds reached the
// remote.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

type boolFixture struct {
	repo, bare string
}

func newBoolFixture(t *testing.T) boolFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	f := boolFixture{repo: filepath.Join(root, "work"), bare: filepath.Join(root, "up.git")}
	for _, d := range []string{f.repo, f.bare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, f.bare, "init", "-q", "--bare")
	gitIn(t, f.repo, "init", "-q", "-b", "main")
	gitIn(t, f.repo, "config", "user.name", "t")
	gitIn(t, f.repo, "config", "user.email", "t@example.com")
	gitIn(t, f.repo, "config", "commit.gpgsign", "false")
	gitIn(t, f.repo, "config", "push.default", "current")
	gitIn(t, f.repo, "commit", "-q", "--allow-empty", "-m", "init")
	gitIn(t, f.repo, "branch", "other")
	gitIn(t, f.repo, "remote", "add", "up", f.bare)
	return f
}

// published reports whether running `git push up` for real, with the given config written to the repo
// and the given -c values on the command line, put the branch `other` on the remote: the thing a
// mirror push adds to a push of the current branch.
func (f boolFixture) published(t *testing.T, fileLine string, cmdC []string) bool {
	t.Helper()
	gitIn(t, f.bare, "update-ref", "-d", "refs/heads/other")
	gitIn(t, f.bare, "update-ref", "-d", "refs/heads/main")
	cfg := filepath.Join(f.repo, ".git", "config")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a pristine copy so each case starts from the same config.
	orig := string(data)
	defer func() { _ = os.WriteFile(cfg, []byte(orig), 0o644) }()
	if fileLine != "" {
		if err := os.WriteFile(cfg, []byte(orig+"\n[remote \"up\"]\n\t"+fileLine+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-C", f.repo}
	for _, c := range cmdC {
		args = append(args, "-c", c)
	}
	args = append(args, "push", "up")
	_ = exec.Command("git", args...).Run() // a refusal (an unreadable boolean) publishes nothing
	out, _ := exec.Command("git", "-C", f.bare, "for-each-ref", "refs/heads/other").Output()
	return strings.Contains(string(out), "refs/heads/other")
}

var boolSpellings = []string{
	"true", "True", "TRUE", "yes", "Yes", "on", "ON", "1", "2", "42", "-1", "0x1", "1k", "10m",
	"false", "False", "no", "off", "0", "-0", "0x0", "", "maybe", "2x",
}

func TestGitBoolean_MirrorSpellingsAgreeWithRealGit(t *testing.T) {
	f := newBoolFixture(t)
	for _, v := range boolSpellings {
		fileLine, cmdC := "mirror = "+v, "remote.up.mirror="+v
		if truth, got := f.published(t, fileLine, nil), f.guardReadsFile(t, fileLine); got != truth {
			t.Errorf("repo config, mirror=%q: git publishes the other branch = %v, the guard reads the key as true = %v", v, truth, got)
		}
		truth := f.published(t, "", []string{cmdC})
		if got := (pushConfig{repo: f.repo, cmd: []string{cmdC}}).boolean("remote.up.mirror"); got != truth {
			t.Errorf("-c, mirror=%q: git publishes the other branch = %v, the guard reads the key as true = %v", v, truth, got)
		}
	}
	// A key with no value at all is true in git.
	if truth := f.published(t, "mirror", nil); !truth {
		t.Fatal("setup: a valueless mirror key should publish everything under real git")
	}
	if !f.guardReadsFile(t, "mirror") {
		t.Error("a valueless `mirror` key in the repo's config is true in git and must widen")
	}
	if truth := f.published(t, "", []string{"remote.up.mirror"}); !truth {
		t.Fatal("setup: a valueless -c key should publish everything under real git")
	}
	if !(pushConfig{repo: f.repo, cmd: []string{"remote.up.mirror"}}).boolean("remote.up.mirror") {
		t.Error("`-c remote.up.mirror` with no value is true in git and must widen")
	}
	// The key unset, and false overridden by the command line, and true overridden to false.
	if (pushConfig{repo: f.repo}).boolean("remote.up.mirror") {
		t.Error("an unset key must not widen")
	}
	if f.guardReadsFileWithCmd(t, "mirror = true", []string{"remote.up.mirror=false"}) {
		t.Error("-c false after a true file value must not widen (the command line wins)")
	}
	if !f.guardReadsFileWithCmd(t, "mirror = false", []string{"remote.up.mirror=2"}) {
		t.Error("-c 2 after a false file value must widen")
	}
}

// guardReadsFile writes the line into the repo's config while the guard asks, then restores it.
func (f boolFixture) guardReadsFile(t *testing.T, line string) bool {
	return f.guardReadsFileWithCmd(t, line, nil)
}

func (f boolFixture) guardReadsFileWithCmd(t *testing.T, line string, cmd []string) bool {
	t.Helper()
	cfg := filepath.Join(f.repo, ".git", "config")
	orig, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(cfg, orig, 0o644) }()
	if err := os.WriteFile(cfg, append(append([]byte{}, orig...), []byte("\n[remote \"up\"]\n\t"+line+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return pushConfig{repo: f.repo, cmd: cmd}.boolean("remote.up.mirror")
}

// The widening value for what the text cannot show: an expansion in a command-line value.
func TestGitBoolean_AnUnreadableCommandLineValueWidens(t *testing.T) {
	f := newBoolFixture(t)
	for _, kv := range []string{"remote.up.mirror=$M", "remote.up.mirror=`cat x`", "remote.up.mirror=?"} {
		if !(pushConfig{repo: f.repo, cmd: []string{kv}}).boolean("remote.up.mirror") {
			t.Errorf("%q cannot be shown not to be true, so it must widen", kv)
		}
	}
}

// push.followTags is the second reader of git's boolean (229). Ground truth again is git: an annotated
// tag reachable from the pushed branch reaches the remote exactly when git reads the key as true.
func (f boolFixture) tagPublished(t *testing.T, section, line string, cmdC []string) bool {
	t.Helper()
	gitIn(t, f.bare, "update-ref", "-d", "refs/tags/v1")
	gitIn(t, f.bare, "update-ref", "-d", "refs/heads/main")
	cfg := filepath.Join(f.repo, ".git", "config")
	orig, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(cfg, orig, 0o644) }()
	if line != "" {
		if err := os.WriteFile(cfg, append(append([]byte{}, orig...), []byte("\n["+section+"]\n\t"+line+"\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-C", f.repo}
	for _, c := range cmdC {
		args = append(args, "-c", c)
	}
	args = append(args, "push", "up", "main")
	_ = exec.Command("git", args...).Run()
	out, _ := exec.Command("git", "-C", f.bare, "for-each-ref", "refs/tags/v1").Output()
	return strings.Contains(string(out), "refs/tags/v1")
}

func TestGitBoolean_FollowTagsSpellingsAgreeWithRealGit(t *testing.T) {
	f := newBoolFixture(t)
	gitIn(t, f.repo, "tag", "-a", "-m", "release", "v1")
	askFile := func(line string) bool {
		cfg := filepath.Join(f.repo, ".git", "config")
		orig, _ := os.ReadFile(cfg)
		defer func() { _ = os.WriteFile(cfg, orig, 0o644) }()
		_ = os.WriteFile(cfg, append(append([]byte{}, orig...), []byte("\n[push]\n\t"+line+"\n")...), 0o644)
		return pushConfig{repo: f.repo}.boolean("push.followTags")
	}
	for _, v := range boolSpellings {
		line := "followTags = " + v
		if truth, got := f.tagPublished(t, "push", line, nil), askFile(line); got != truth {
			t.Errorf("repo config, followTags=%q: git publishes the tag = %v, the guard reads the key as true = %v", v, truth, got)
		}
		c := "push.followTags=" + v
		if truth, got := f.tagPublished(t, "", "", []string{c}), (pushConfig{repo: f.repo, cmd: []string{c}}).boolean("push.followTags"); got != truth {
			t.Errorf("-c, followTags=%q: git publishes the tag = %v, the guard reads the key as true = %v", v, truth, got)
		}
	}
	// A key with no value is true, in the file and on the command line.
	if !f.tagPublished(t, "push", "followTags", nil) || !askFile("followTags") {
		t.Error("a valueless followTags in the repo's config is true in git and must be read as true")
	}
	if !f.tagPublished(t, "", "", []string{"push.followTags"}) || !(pushConfig{repo: f.repo, cmd: []string{"push.followTags"}}).boolean("push.followTags") {
		t.Error("`-c push.followTags` with no value is true in git and must be read as true")
	}
	// And the guard refuses on the hook's own path, end to end, for a spelling the word list missed.
	if why := followTagsConfigured(f.repo, nil, []string{"push.followTags=2"}); why == "" {
		t.Error("push.followTags=2 publishes tags under git and must be refused")
	}
	if why := followTagsConfigured(f.repo, []string{"--no-follow-tags"}, []string{"push.followTags=2"}); why != "" {
		t.Error("--no-follow-tags turns it off and must not be refused")
	}
}

// The override is the OPTION --no-follow-tags, not the text: a push option's value, or any word an option
// consumes, can spell it without being it. Real git is the ground truth again: with push.followTags set
// and a server that takes push options, the tag reaches the remote exactly when git did not read a real
// --no-follow-tags, and the guard must refuse in every case where it does.
func TestFollowTags_OnlyARealNegationOptionOverridesTheConfig(t *testing.T) {
	f := newBoolFixture(t)
	gitIn(t, f.bare, "config", "receive.advertisePushOptions", "true")
	gitIn(t, f.repo, "tag", "-a", "-m", "release", "v1")
	gitIn(t, f.repo, "config", "push.followTags", "true")
	cases := []struct {
		name string
		args []string // after `push`
		real bool     // --no-follow-tags is a real option here
	}{
		{"before the remote", []string{"--no-follow-tags", "up", "main"}, true},
		{"after the refspec", []string{"up", "main", "--no-follow-tags"}, true},
		{"between other options", []string{"-u", "--no-follow-tags", "-q", "up", "main"}, true},
		{"as -o's value", []string{"-o", "--no-follow-tags", "up", "main"}, false},
		{"as --push-option's value", []string{"--push-option", "--no-follow-tags", "up", "main"}, false},
		{"as a -o cluster's value", []string{"-uo", "--no-follow-tags", "up", "main"}, false},
		{"as --exec's value", []string{"--exec", "--no-follow-tags", "up", "main"}, false},
		{"as --receive-pack's value", []string{"--receive-pack", "--no-follow-tags", "up", "main"}, false},
		{"inline push option", []string{"--push-option=--no-follow-tags", "up", "main"}, false},
	}
	for _, c := range cases {
		gitIn(t, f.bare, "update-ref", "-d", "refs/tags/v1")
		gitIn(t, f.bare, "update-ref", "-d", "refs/heads/main")
		_ = exec.Command("git", append([]string{"-C", f.repo, "push"}, c.args...)...).Run()
		out, _ := exec.Command("git", "-C", f.bare, "for-each-ref", "refs/tags/v1").Output()
		published := strings.Contains(string(out), "refs/tags/v1")
		refused := followTagsConfigured(f.repo, c.args, nil) != ""
		if published && !refused {
			t.Errorf("%s: git published the tag and the guard allowed it: %v", c.name, c.args)
		}
		if c.real && (published || refused) {
			t.Errorf("%s: a real --no-follow-tags must override (git published=%v, guard refused=%v): %v", c.name, published, refused, c.args)
		}
		if !c.real && !refused {
			t.Errorf("%s: a word an option consumed is not the override, but the guard allowed %v", c.name, c.args)
		}
	}
}
