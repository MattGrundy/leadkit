// External per-project config resolution (docs/build-tasks/external-config-resolution.md):
// leadkit's config may sit OUTSIDE a repo, keyed by the repo's cwd-honest identity slug,
// at ~/.claude/leadkit/config/<slug>.yaml. This is the THIRD resolution step, tried ONLY
// when neither $LEADKIT_PROFILE nor an in-repo leadkit.yaml walk-up resolves — so the
// in-repo path is UNCHANGED and wins for back-compat; external is purely additive.
//
// Two load-bearing invariants (see the build-task doc):
//   - an externally-loaded profile's ProjectRoot() is the ACTUAL repo it was resolved
//     FOR (its git-toplevel/cwd), NOT the external config file's directory — so gate
//     own-origin derives the REAL repo's remote and identity maps to the real project.
//     Threaded through as Profile.externalRepoRoot.
//   - relative paths inside an external config (queue_root: ./queue, ...) anchor to the
//     EXTERNAL CONFIG's own directory (Profile.root stays the config dir), keeping the
//     queue/store out of the repo too. So Resolve() and ProjectRoot() are DELIBERATELY
//     different for an external-loaded profile.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/usage"
)

// externalConfigDir returns the directory external per-project configs are looked up
// in — usage.DefaultConfigDir by default, but recomputed from $HOME at call time so a
// test (or an adopter) that relocates HOME resolves the right dir. An injectable func
// var: tests point it at a temp dir. An empty HOME falls back to the package default.
var externalConfigDir = func() string {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".claude", "leadkit", "config")
	}
	return usage.DefaultConfigDir
}

// ExternalConfigDir is the exported accessor for the directory external per-project
// configs live in (~/.claude/leadkit/config, honoring $HOME at call time) — used by
// `leadkit config init --external` to write the starter, and by anything reporting the
// external location. Mirrors usage.DefaultConfigDir's value.
func ExternalConfigDir() string { return externalConfigDir() }

// gitToplevelDir resolves the git working-tree root that CONTAINS dir — the cwd-honest
// identity anchor the external slug is keyed on. ok=false when dir is not inside a git
// repo, or git can't run: the caller then keeps dir itself as the identity root (never
// an ancestor's, never an env profile's). An injectable var so tests are deterministic
// without a real git binary. Mirrors cmd/leadkit's own gitToplevel — kept here so the
// config package resolves identity without importing the main package.
var gitToplevelDir = realGitToplevelDir

// realGitToplevelDir is gitToplevelDir's production implementation (a named func so a test
// that stubs the var can restore the real behaviour).
func realGitToplevelDir(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	top := trimTrailingSpace(string(out))
	if top == "" {
		return "", false
	}
	return top, true
}

// gitOriginURLDir resolves the git remote.origin.url of the repo that CONTAINS dir —
// the stable identity anchor the external slug is now keyed on. It reads the `origin`
// remote SPECIFICALLY (not the first remote), and a worktree returns its shared main
// repo's origin, so the key is the same from every clone/worktree of a repo. "" on any
// failure (no repo, no origin, git absent) → the caller falls back to the basename key.
// An injectable var so tests are deterministic without a real git binary. Mirrors
// cmd/leadkit's gitOriginRemote — kept here so the config package resolves identity
// without importing the main package.
var gitOriginURLDir = func(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return trimTrailingSpace(string(out))
}

// originSlugForRoot derives the ORIGIN-based identity slug for the repo rooted at
// repoRoot — the git origin's repo-name (stable across every clone/worktree), else the
// repoRoot basename when there is no resolvable origin (a local-only repo) or the origin
// URL is malformed. This keeps the external config key stable across clones/worktrees:
// ~/.claude/leadkit/config/<origin-name>.yaml resolves from ANY checkout of the repo.
func originSlugForRoot(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	if name := install.OriginRepoName(gitOriginURLDir(repoRoot)); name != "" {
		return name
	}
	return install.Slugify(filepath.Base(repoRoot))
}

// trimTrailingSpace trims trailing whitespace/newlines from git's output without
// pulling in strings just for a TrimSpace (external.go stays import-light).
func trimTrailingSpace(s string) string {
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// repoIdentityRoot returns the cwd-honest identity root for startDir: its git toplevel
// when startDir is inside a repo (read from the filesystem when git cannot answer), else
// startDir itself. This is the ACTUAL repo an external config governs — NEVER an ancestor
// leadkit.yaml's dir and NEVER an env profile's. Empty startDir yields "".
func repoIdentityRoot(startDir string) string {
	if startDir == "" {
		return ""
	}
	if top, ok := gitToplevelDir(startDir); ok && top != "" {
		return top
	}
	if abs, err := filepath.Abs(startDir); err == nil {
		if top, ok := repoRootOf(abs); ok {
			return canonicalDir(top)
		}
	}
	return startDir
}

// ExternalConfigPathForDir computes the external per-project config path for the repo
// that CONTAINS startDir, keyed by that repo's ORIGIN-based identity slug (the git
// origin's repo-name, slugified the SAME way daemon/registry/identity slugs are — reused,
// never forked; the repoRoot basename only when there is no resolvable origin). Keying by
// origin makes the config resolve from ANY clone/worktree of a repo, not just one checkout
// dir. It returns (configPath, repoRoot, ok):
//   - configPath is ~/.claude/leadkit/config/<slug>.yaml, and ok is true ONLY when that
//     file actually exists (a regular file);
//   - repoRoot is the ACTUAL repo the config governs (git-toplevel of startDir, else
//     startDir) — what an external profile's ProjectRoot() must report so gate own-origin
//     and identity resolve against the real repo, not the external config dir.
//
// It NEVER walks up to an ancestor leadkit.yaml and NEVER reads $LEADKIT_PROFILE: the
// slug is derived purely from startDir's own repo (origin repo-name, else git-toplevel/cwd
// basename), so there is no env/ancestor leak. ok is false for an empty startDir, an empty
// slug, or a missing file.
func ExternalConfigPathForDir(startDir string) (configPath, repoRoot string, ok bool) {
	cand, repoRoot, _ := ExternalConfigTargetForDir(startDir)
	if cand == "" {
		return "", repoRoot, false
	}
	fi, err := os.Stat(cand)
	if err != nil || fi.IsDir() {
		return "", repoRoot, false
	}
	return cand, repoRoot, true
}

// ExternalConfigTargetForDir is ExternalConfigPathForDir's derivation WITHOUT the
// existence check: the path the external config for the repo containing startDir WOULD
// live at (~/.claude/leadkit/config/<origin-slug>.yaml), whether or not it exists yet, plus
// the repo root and the slug. ExternalConfigPathForDir is built on it, so `config path`,
// doctor and whoami can never name a different file than Load reads. configPath is ""
// for an empty startDir or empty slug.
func ExternalConfigTargetForDir(startDir string) (configPath, repoRoot, slug string) {
	repoRoot = repoIdentityRoot(startDir)
	if repoRoot == "" {
		return "", "", ""
	}
	slug = originSlugForRoot(repoRoot)
	if slug == "" {
		return "", repoRoot, ""
	}
	return filepath.Join(externalConfigDir(), slug+".yaml"), repoRoot, slug
}

// StarterConfig returns the starter leadkit config `leadkit config init` writes for
// slug — mirroring docs/adopter-profiles/per-project-template.leadkit.yaml (identity +
// own queue + own-origin gate scope + push whitelist + a roster stub). external=true
// adds the load-bearing note that RELATIVE paths (queue_root: ./queue, ...) anchor to
// THIS config file's own dir (~/.claude/leadkit/config/), keeping the queue/store out of
// the repo too — while the gate still scopes to the REAL repo's origin. Authority grants
// are deliberately absent (Matt-direct, never scaffolded).
func StarterConfig(slug string, external bool) string { return StarterConfigFor(slug, external, "") }

// StarterConfigFor is StarterConfig plus, for an external config whose repo has a resolvable
// `owner/repo` origin, the project_origin line that stops a same-named clone of someone else's
// repo loading it (see Profile.ProjectOrigin).
func StarterConfigFor(slug string, external bool, projectOrigin string) string {
	originLine := ""
	if external && projectOrigin != "" {
		originLine = "\n# ORIGIN — this config is found by the repo's NAME; a clone of a different owner's repo\n" +
			"# with the same name is refused unless its origin matches this:\nproject_origin: " + projectOrigin + "\n"
	}
	loc := "Place this at the ROOT of the project's own repo/clone (in-repo)."
	relNote := "# Relative paths (queue_root: ./queue) anchor to THIS repo root."
	if external {
		loc = "External per-project config — resolved for the repo whose cwd-honest slug is\n" +
			"# " + slug + ", when that repo has NO committed leadkit.yaml. Lives OUTSIDE the repo."
		relNote = "# NOTE: because this config is EXTERNAL, relative paths (queue_root: ./queue)\n" +
			"# anchor to THIS config file's own dir (~/.claude/leadkit/config/), keeping the\n" +
			"# queue/store OUT of the repo. The review gate still scopes to the REAL repo's\n" +
			"# origin. Use an ABSOLUTE path to place the queue/store elsewhere."
	}
	return fmt.Sprintf(`# ─────────────────────────────────────────────────────────────────────────────
# leadkit config for %s. %s
# ─────────────────────────────────────────────────────────────────────────────

# IDENTITY — this session's honest slug + inbox (wins over the queue_root basename).
project_id: %s
%s
# QUEUE — this project's own queue.
%s
queue_root: ./queue

# GATE SCOPE — require the review gate before pushing THIS project's own repo.
# The repo's OWN origin is always gated (auto-derived from its git remote), whatever
# enforced_remotes holds. List here only OTHER repos' origin-URL substrings; naming your
# own repo as well is harmless:
enforced_remotes:
  - %s

# PUSH BRANCHES — which branches a raw push may reach; protected branches still need PR.
push_whitelist: [feat/*, fix/*, chore/*, mg/*, user/*]

# ROSTER — declare this project's work-roles (add/trim to match your team).
roster:
  - type: <role>
    count: 1

# AUTHORITY — PR-create/merge grants are Matt-direct (never scaffolded here).
`, slug, loc, slug, originLine, relNote, slug)
}

// loadLayeredExternal loads an EXTERNAL per-project config the SAME layered way
// loadLayered loads an in-repo one, with two deliberate differences: Profile.root (the
// anchor Resolve() joins relative paths against) is the CONFIG FILE's own directory (so
// queue_root: ./queue lands next to the config, out of the repo), while
// Profile.externalRepoRoot — what ProjectRoot() reports — is the REAL repo the config was
// resolved FOR (so gate own-origin + identity use the repo's remote/basename). The
// user/team layers are consulted exactly as for an in-repo project (team_config resolves
// relative to the config dir, same as its own relative paths).
func loadLayeredExternal(configPath, repoRoot string) (*Profile, map[string]KeyOrigin, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: read external config %s: %w", configPath, err)
	}
	anchorDir := filepath.Dir(configPath)

	userData, err := loadLayerFile(discoverUserConfigPath())
	if err != nil {
		return nil, nil, fmt.Errorf("config: read user config: %w", err)
	}
	teamPath := resolveTeamConfigPath(data, anchorDir)
	teamData, err := loadLayerFile(teamPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: read team config (%s): %w", teamPath, err)
	}

	prof, origins, err := resolveLayers(userData, teamData, data, anchorDir)
	if err != nil {
		return nil, nil, err
	}
	// ProjectRoot() diverges from root ONLY for an external config: the real repo, not
	// the config dir. resolveLayers already set prof.root = anchorDir (the config dir).
	prof.externalRepoRoot = repoRoot
	if prof.ProjectOrigin != "" {
		if origin := gitOriginURLDir(repoRoot); !OriginMatchesProject(origin, prof.ProjectOrigin) {
			return nil, nil, &originMismatchError{msg: fmt.Sprintf("config: %s declares project_origin %q, but the repo at %s has origin %q; refusing to load another repo's store and queue paths", configPath, RedactOrigin(prof.ProjectOrigin), repoRoot, RedactOrigin(origin))}
		}
	}
	return prof, origins, nil
}

// OriginMatchesProject reports whether a repo's origin URL is the repo that key names. key is
// `owner/repo` or `host/owner/repo`, and it matches the URL's tail at a path boundary, so
// `acme/widgets` matches git@github.com:acme/widgets.git but neither `other/widgets` nor
// `me/acme/widgets-fork`. Case-sensitive: a key that differs only in case is a different repo
// to a host that distinguishes it, and failing closed costs only a re-run of `config init`. A
// key without an owner (`widgets`) would match any same-named repo, which is the exposure being
// closed, so it matches nothing.
func OriginMatchesProject(origin, key string) bool {
	o, k := normalizeOrigin(origin), normalizeOrigin(key)
	if !strings.Contains(k, "/") {
		return false
	}
	return o == k || strings.HasSuffix(o, "/"+k)
}

// ProjectOriginForDir is the project_origin value for the repo containing dir: its origin as
// `owner/repo` (the last two path segments), "" when it has no origin or one with no owner.
func ProjectOriginForDir(dir string) string {
	n := normalizeOrigin(gitOriginURLDir(repoIdentityRoot(dir)))
	parts := strings.Split(n, "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// stripUserinfo removes the `user[:password]@` of a git URL, in both the URL form
// (`https://user:token@host/path`: the userinfo is whatever precedes the LAST `@` of the
// authority, up to the first `/`) and the scp form (`token@host:path`). A remote can carry a
// credential there, so nothing that prints or compares an origin keeps it.
func stripUserinfo(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		scheme, rest := u[:i+3], u[i+3:]
		auth := rest
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			auth = rest[:slash]
		}
		if at := strings.LastIndexByte(auth, '@'); at >= 0 {
			rest = rest[at+1:]
		}
		return scheme + rest
	}
	at, colon, slash := strings.IndexByte(u, '@'), strings.IndexByte(u, ':'), strings.IndexByte(u, '/')
	if at >= 0 && (colon < 0 || at < colon) && (slash < 0 || at < slash) {
		return u[at+1:]
	}
	return u
}

// RedactOrigin is how an origin URL is shown in a message: host and path, no scheme, no
// userinfo. Use it wherever a remote is formatted into text an agent or a log will see.
func RedactOrigin(u string) string { return normalizeOrigin(u) }

// normalizeOrigin reduces a git URL (https, ssh://, scp-style) or a project_origin key to
// `host/owner/repo` form: no scheme, no userinfo, scp's `:` as `/`, no `.git`, no trailing slash.
func normalizeOrigin(u string) string {
	u = stripUserinfo(strings.TrimSpace(u))
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, ":"); i >= 0 && !strings.Contains(u[:i], "/") {
		u = u[:i] + "/" + u[i+1:]
	}
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	return strings.TrimLeft(u, "/")
}

// originMismatchError is the refusal of an external config whose project_origin does not match the
// repo it was found for: the config is some other repo's, not a failure to read the operator's own.
type originMismatchError struct{ msg string }

func (e *originMismatchError) Error() string { return e.msg }

// grantKeys are the profile keys that GRANT permission: which branches a raw push may reach, which
// repos hold which authority, which remotes the gate also covers, and the ownership boundary; and
// the two that decide whether the gate stands at all, the enforcement mode of each guard and the
// directory the review gate reads its records and its BYPASS file from (a committed records_dir
// holding a BYPASS file authorises every push, and a committed advisory mode turns a guard the
// operator made fail-closed back into one that fails open, silently). Everything else in a profile
// DESCRIBES a project (its paths, roster, queue).
//
// A key is a grant if a committed value for it can make something allowed that the operator's
// config would refuse, or switch a check off. Classify a new key when it is added;
// TestGrantKeys_EveryEntryIsTakenFromTheOperatorConfig fails for an entry setGrantKeys does not copy.
var grantKeys = []string{"push_whitelist", "authority", "enforced_remotes", "owners_file", "records_dir", "guards"}

// overlayOperatorGrants makes the operator's external config the only source of the grant keys
// when one exists for the repo containing startDir. prof is a COMMITTED profile (found by walking
// up in the repo): anyone who can land a commit can land that file, so under in-repo-wins a
// branch could carry a permissive push_whitelist and authorise its own push. The committed file
// still describes the project; its grant keys are replaced by the external config's, including
// when the external config says nothing (it then supplies the defaults), so the committed value
// can never widen what the operator allows.
//
// An external config whose project_origin does not match this clone belongs to another repo (a
// same-named clone of someone else's). That is no reason to accept the clone's own committed
// grants: a mismatch means "this is not the repo I was written for", which withholds the
// operator's grants and does not hand the decision to the object being governed. The grant
// keys then take the built-in defaults (the default whitelist, no authority, so default-deny, no
// extra enforced remotes, no owners file); every other key still comes from the committed file and
// the load is not refused.
//
// The ONLY case where a committed file supplies grant keys is a machine with no external config
// for the repo at all: an adopter who commits a profile and has no operator to defer to governs
// itself, which is the agnostic design. That is deliberate and is the line this function draws.
//
// owners_file is made absolute against the external config's own directory, as that config
// resolves its relative paths, so it does not re-anchor to the committed file's repo root.
func overlayOperatorGrants(prof *Profile, origins map[string]KeyOrigin, startDir string) error {
	cand, repoRoot, ok := ExternalConfigPathForDir(startDir)
	if !ok {
		return nil
	}
	ext, _, err := loadLayeredExternal(cand, repoRoot)
	if err != nil {
		var mismatch *originMismatchError
		if !errors.As(err, &mismatch) {
			return err
		}
		d := Defaults()
		d.root = prof.root
		setGrantKeys(prof, origins, &d, "default")
		return nil
	}
	setGrantKeys(prof, origins, ext, "external")
	return nil
}

// ApplyOperatorGrants applies the operator's external config's grant keys to prof, as the load of a
// committed profile does (overlayOperatorGrants), for a profile built some other way: the stand-in
// the file guards use when the committed profile did not load. applied is false when the repo
// containing startDir has no external config, so the profile's own values stand.
func ApplyOperatorGrants(prof *Profile, startDir string) (applied bool, err error) {
	if _, _, ok := ExternalConfigPathForDir(startDir); !ok {
		return false, nil
	}
	return true, overlayOperatorGrants(prof, nil, startDir)
}

// setGrantKeys replaces prof's grant keys with src's and attributes them to layer.
func setGrantKeys(prof *Profile, origins map[string]KeyOrigin, src *Profile, layer string) {
	prof.PushWhitelist = src.PushWhitelist
	prof.Authority = src.Authority
	prof.EnforcedRemotes = src.EnforcedRemotes
	prof.OwnersFile = ""
	if src.OwnersFile != "" {
		prof.OwnersFile = src.Resolve(src.OwnersFile)
	}
	// Resolved against src like owners_file: a relative records_dir must not re-anchor to the committed
	// file's repo root, where the BYPASS file would be repo content.
	prof.RecordsDir = src.Resolve(src.RecordsDir)
	prof.Guards = src.Guards
	for _, k := range grantKeys {
		if origins != nil {
			origins[k] = KeyOrigin{Layer: layer}
		}
	}
}
