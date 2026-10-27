package install

// The daemon/cron part of `leadkit init`: two macOS LaunchAgents — a waker
// (`leadkit queue watch`, KeepAlive so it just keeps running) and a periodic sweep
// (`leadkit queue sweep`, fired on the profile's heartbeat schedule). Linux/systemd
// is a TODO (ComputeDaemonPlan reports "not yet supported" and skips, never errors
// the whole init).
//
// NOTE — this is NOT the session heartbeat: the SESSION-local heartbeat (CronCreate,
// re-armed each session by the SessionStart skill, also driven off
// heartbeat.schedule) is a completely different mechanism, owned by that skill. These
// two LaunchAgents are the OS-level services that persist across sessions/reboots.
//
// Same safety model as the hooks part (see install.go's package doc): ComputeDaemonPlan
// is pure (reads the target plist paths to detect "already installed, unchanged", but
// writes nothing); only ApplyDaemonPlan (called for --apply --confirm) writes the
// plist files (atomically, with a timestamped backup of whatever it replaces) and
// loads them — via the injectable runLaunchctl func var, so tests never shell out.
//
// PER-PROJECT NAMESPACING (docs/build-tasks/per-project-daemons.md): the waker/sweep
// are inherently per-QUEUE (they take --queue <path>), so every project running its
// own team needs its OWN pair of LaunchAgents — a fixed "dev.leadkit.waker"/
// "dev.leadkit.sweep" label+filename would let a second project's `init` silently
// clobber the first's. ResolveProjectSlug derives a stable per-project slug (an
// explicit project_id profile key, else the queue_root's project-root basename) and
// ComputeDaemonPlan folds it into both the plist Label and its filename:
// "dev.leadkit.waker.<slug>" / "dev.leadkit.sweep.<slug>". An unresolvable slug falls
// back to the historical bare label (single-project back-compat) and surfaces a
// Warning (DaemonPlan.Warning) — a silent collision is exactly the failure mode this
// exists to prevent. Run `leadkit init` from a project with a resolvable slug (or an
// explicit project_id) whenever more than one project's team will share a machine.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WakerLabelBase and SweepLabelBase are the bare (un-slugged) label roots every
// project's daemon labels are built from — exported so doctor can recognise a
// project's own agents (and a bare legacy pair) without duplicating the string.
const (
	WakerLabelBase = "dev.leadkit.waker"
	SweepLabelBase = "dev.leadkit.sweep"

	// BurnLabel is the ACCOUNT-LEVEL burn-enforce daemon
	// (docs/build-tasks/burn-auto-enforce.md). Unlike the waker/sweep it is NOT
	// per-project namespaced: the throttle it enforces is account-scoped (the weekly
	// budget is account-wide), so exactly ONE such daemon serves the whole machine —
	// every project's `leadkit init` writes the SAME `dev.leadkit.burn.plist`
	// (byte-identical content), which is idempotent by construction, and no slug can
	// collide because there is deliberately no slug.
	BurnLabel = "dev.leadkit.burn"
)

// slugSafe matches the characters ResolveProjectSlug's slug is allowed to contain — a
// valid launchd label segment: lowercase letters, digits, and hyphens.
var slugSafe = func(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}

// slugify lowercases s and collapses every run of characters outside [a-z0-9-] to a
// single "-", trimming any leading/trailing "-" left over. Never panics on empty or
// all-symbol input — both simply produce "".
func slugify(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range lower {
		if slugSafe(r) && r != '-' {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		// r == '-' or any other rune: both collapse into a single separating dash,
		// but never two dashes in a row.
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// Slugify is the exported form of the package's slug normalizer. The session-IDENTITY
// resolver (cmd/leadkit) derives a git-repo/dir basename slug through the SAME
// normalization ResolveProjectSlug already applies to project_id/queue_root — reused,
// never forked, so identity and daemon/registry slugs can't drift.
func Slugify(s string) string { return slugify(s) }

// OriginRepoName derives a repository's stable identity name from its git
// remote.origin.url: the URL's LAST path segment with a trailing ".git" stripped,
// then slugified the SAME way every other leadkit slug is. This is the identity a
// repo IS (its origin), stable across every clone/worktree/checkout-dir name — so a
// worktree (whose git-toplevel basename is a branch/chore name) and a differently
// named clone both resolve to one slug.
//
// It parses all the common URL forms to the same name:
//   - ssh scp-style:  git@host:owner/name.git      -> name
//   - https:          https://host/owner/name.git  -> name
//   - no-.git:        https://host/owner/name       -> name
//   - ssh:// / git://: ssh://host/owner/name.git    -> name
//
// The last path segment is everything after the final '/' or ':' (scp-style uses ':'
// between host and path). A trailing "/" is tolerated. An empty or unparseable URL —
// or one that slugifies to nothing — returns "" so callers fall back to the basename
// derivation (this never crashes on a malformed origin).
func OriginRepoName(originURL string) string {
	s := strings.TrimSpace(originURL)
	if s == "" {
		return ""
	}
	// Tolerate a trailing slash so ".../name/" still yields "name".
	s = strings.TrimRight(s, "/")
	// The repo name is the final path segment: everything after the last '/' or ':'
	// ('/' for URL paths, ':' for the scp-style host:path separator).
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	// Strip a single trailing ".git" before slugifying (otherwise the '.' would
	// collapse to a dash and leave a "-git" suffix).
	s = strings.TrimSuffix(s, ".git")
	return slugify(s)
}

// ResolveProjectSlug derives the per-project slug ComputeDaemonPlan namespaces its
// daemon labels/filenames with. An explicit projectID (the profile's project_id key)
// wins outright when it slugifies to something non-empty. Otherwise the slug is
// derived from queueRoot's PROJECT-ROOT basename — the directory queueRoot itself
// sits in (e.g. ".../my-project/queue" -> "my-project"), not
// queueRoot's own basename (which is almost always the uninformative "queue"). Empty
// or unresolvable input (no project_id and an empty/rootless queueRoot) returns "" —
// callers must treat that as "keep the bare legacy label" and surface a warning,
// since an empty slug is exactly the collision-prone, single-project-only case.
func ResolveProjectSlug(projectID, queueRoot string) string {
	if s := slugify(projectID); s != "" {
		return s
	}
	if queueRoot == "" {
		return ""
	}
	base := filepath.Base(filepath.Dir(filepath.Clean(queueRoot)))
	return slugify(base)
}

// daemonLabel builds the (possibly namespaced) label for base ("dev.leadkit.waker" or
// "dev.leadkit.sweep") — base.<slug>, or bare base when slug is "".
func daemonLabel(base, slug string) string {
	if slug == "" {
		return base
	}
	return base + "." + slug
}

// unresolvedSlugWarning is DaemonPlan.Warning's text whenever ResolveProjectSlug came
// back empty — surfaced by FormatDaemonPlan (and available to `doctor`) so the
// collision risk is never silent.
func unresolvedSlugWarning(queueRoot string) string {
	return fmt.Sprintf(
		"no project_id set and queue_root %q has no resolvable project-root basename — "+
			"falling back to the bare %s/%s labels. A second project's `leadkit init` WILL "+
			"silently overwrite these. Set an explicit project_id (or use a queue_root nested "+
			"under a named project directory) to avoid collisions.",
		queueRoot, WakerLabelBase, SweepLabelBase,
	)
}

// CalendarTime is a launchd StartCalendarInterval's Hour/Minute (every day at
// HH:MM) — the one calendar shape cronToLaunchd translates to; anything richer falls
// back to StartIntervalSec instead.
type CalendarTime struct {
	Hour   int
	Minute int
}

// PlistSpec is the subset of a launchd job dict leadkit's two services need.
type PlistSpec struct {
	Label             string
	ProgramArguments  []string
	KeepAlive         bool
	RunAtLoad         bool
	StandardOutPath   string
	StandardErrorPath string
	// WorkingDirectory is launchd's cwd for the job (omitted from the plist when
	// empty). Set to the project root so any cwd-relative resolution in the sweep/
	// waker is correct even though launchd's default cwd is `/` — defense in depth
	// behind the sweep's own `--queue`-derived scoping.
	WorkingDirectory string
	// Exactly one of these should be set (StartCalendar wins if both are, which
	// ComputeDaemonPlan never does in practice — cronToLaunchd returns one or the
	// other, never both).
	StartIntervalSec int
	StartCalendar    *CalendarTime
}

// RenderPlist renders spec as a launchd property-list XML document — hand-built
// (stdlib only, no plist library dependency) since the shape needed here is small
// and fixed. Every string value is XML-escaped via encoding/xml.
func RenderPlist(spec PlistSpec) []byte {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writeStringKV(&b, "Label", spec.Label)

	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range spec.ProgramArguments {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("\t</array>\n")

	writeBoolKV(&b, "KeepAlive", spec.KeepAlive)
	writeBoolKV(&b, "RunAtLoad", spec.RunAtLoad)

	if spec.StandardOutPath != "" {
		writeStringKV(&b, "StandardOutPath", spec.StandardOutPath)
	}
	if spec.StandardErrorPath != "" {
		writeStringKV(&b, "StandardErrorPath", spec.StandardErrorPath)
	}
	if spec.WorkingDirectory != "" {
		writeStringKV(&b, "WorkingDirectory", spec.WorkingDirectory)
	}

	switch {
	case spec.StartCalendar != nil:
		b.WriteString("\t<key>StartCalendarInterval</key>\n\t<dict>\n")
		fmt.Fprintf(&b, "\t\t<key>Hour</key>\n\t\t<integer>%d</integer>\n", spec.StartCalendar.Hour)
		fmt.Fprintf(&b, "\t\t<key>Minute</key>\n\t\t<integer>%d</integer>\n", spec.StartCalendar.Minute)
		b.WriteString("\t</dict>\n")
	case spec.StartIntervalSec > 0:
		fmt.Fprintf(&b, "\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", spec.StartIntervalSec)
	}

	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func writeStringKV(b *bytes.Buffer, key, val string) {
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<string>%s</string>\n", xmlEscape(key), xmlEscape(val))
}

func writeBoolKV(b *bytes.Buffer, key string, val bool) {
	tag := "false"
	if val {
		tag = "true"
	}
	fmt.Fprintf(b, "\t<key>%s</key>\n\t<%s/>\n", xmlEscape(key), tag)
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// cronToLaunchd translates a 5-field cron expression (profile heartbeat.schedule)
// into a launchd schedule. Only two shapes are recognised — the ones the "simple
// every-N-minutes is fine" spec calls for: "*/N * * * *" (every N minutes ->
// StartIntervalSec) and "M H * * *" (a fixed daily time -> StartCalendarInterval).
// Anything else (day-of-month/month/day-of-week constraints, lists, ranges, steps on
// hour, ...) falls back to a 20-minute StartIntervalSec, with note explaining why —
// callers surface note in the plan so a human sees the approximation being made.
func cronToLaunchd(expr string) (startIntervalSec int, cal *CalendarTime, note string) {
	fallback := func(reason string) (int, *CalendarTime, string) {
		return 1200, nil, fmt.Sprintf("heartbeat.schedule %q %s — falling back to every 20 minutes (StartInterval)", expr, reason)
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fallback("isn't a recognisable 5-field cron expression")
	}
	min, hour, dom, mon, dow := fields[0], fields[1], fields[2], fields[3], fields[4]
	if dom != "*" || mon != "*" || dow != "*" {
		return fallback("uses day-of-month/month/day-of-week fields this simple translator doesn't handle")
	}
	if hour == "*" && strings.HasPrefix(min, "*/") {
		if n, err := strconv.Atoi(strings.TrimPrefix(min, "*/")); err == nil && n > 0 {
			return n * 60, nil, ""
		}
	}
	if h, herr := strconv.Atoi(hour); herr == nil {
		if m, merr := strconv.Atoi(min); merr == nil {
			return 0, &CalendarTime{Hour: h, Minute: m}, ""
		}
	}
	return fallback("has a shape this simple translator doesn't handle")
}

// ServiceSpec is one launchd service leadkit init manages, fully rendered.
type ServiceSpec struct {
	Label     string
	PlistPath string
	Plist     []byte
	Note      string // non-empty only for a cronToLaunchd fallback worth surfacing
}

// ServiceStatus pairs a ServiceSpec with whether it needs writing.
type ServiceStatus struct {
	Spec    ServiceSpec
	Changed bool // false: a file with this EXACT content already sits at Spec.PlistPath
}

// projectProfileName is the filename that marks a project's root — the same file
// config discovers by walking up from cwd (config.DefaultProfileName). Duplicated here
// (rather than importing internal/config) to avoid an import cycle: config imports
// internal/usage, whose tests import internal/install.
const projectProfileName = "leadkit.yaml"

// discoverProjectRoot walks up from start looking for a project leadkit.yaml (the same
// git-style walk config uses) and returns the directory that CONTAINS it — the project
// root ComputeDaemonPlan pins the plists' WorkingDirectory to. When no leadkit.yaml is
// found anywhere above start (a bare/orphan queue), it falls back to start itself: the
// queue root's parent is a best-effort project-root guess (queues sit at
// <project>/queue), and is never worse than launchd's default cwd of `/`.
func discoverProjectRoot(start string) string {
	dir := start
	for {
		cand := filepath.Join(dir, projectProfileName)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// realDaemonQueueRootExists reports whether path is an existing directory — the
// production check ComputeDaemonPlan uses to refuse planning a daemon aimed at a
// non-existent queue root (Fix 3).
func realDaemonQueueRootExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// daemonQueueRootExists is realDaemonQueueRootExists, indirected through a package var
// so the pre-Fix-3 daemon tests (which pass synthetic queue paths like "/q" purely to
// exercise the label/plist planning) can force it true without materialising a real
// dir. Production always uses the real stat.
var daemonQueueRootExists = realDaemonQueueRootExists

// DaemonPlan is the daemon/cron part of `leadkit init`'s plan.
type DaemonPlan struct {
	// Supported is false on any non-darwin GOOS, OR when the resolved queue root
	// doesn't exist (a daemon aimed at a missing queue is skipped, not written).
	Supported bool
	SkipNote  string // set (and Services empty) when !Supported
	// QueueMissing is true when the plan was skipped only because the queue root does not exist yet:
	// the one skip an adopter can fix by running the steps in the other order.
	QueueMissing bool
	Services     []ServiceStatus
	// Slug is the per-project namespace ComputeDaemonPlan resolved (see
	// ResolveProjectSlug) — "" means the bare legacy label is in play.
	Slug string
	// Warning is non-empty only when Slug is "" because it couldn't be resolved
	// (as opposed to deliberately unset) — a collision risk worth surfacing.
	Warning string
}

// SkippedWarning is the loud, unmissable statement that the waker and sweep were NOT planned
// because the queue does not exist yet, or "" when that is not why. It must stand out: a one-line
// SkipNote among a screen of plan output leaves an adopter with no daemons while the guide says init
// installs them.
func (p DaemonPlan) SkippedWarning() string {
	if !p.QueueMissing {
		return ""
	}
	return "WARNING: the waker and sweep daemons were NOT installed. " + p.SkipNote +
		". Create the queue (`leadkit team scaffold --apply --confirm`), then run `leadkit init --daemons-only --confirm`. " +
		"Until then nothing wakes the owners."
}

// AnyChanged reports whether Apply would write anything.
func (p DaemonPlan) AnyChanged() bool {
	for _, s := range p.Services {
		if s.Changed {
			return true
		}
	}
	return false
}

// ComputeDaemonPlan is pure except for the "does this exact content already exist at
// this path" check (a read, never a write) — goos is passed in (rather than read from
// runtime.GOOS internally) so this is testable on any host regardless of what it's
// actually running on. binaryPath MUST be absolute (launchd doesn't reliably see an
// interactive shell's PATH) — see ResolveBinaryPath. projectID is the profile's
// (optional) project_id key — see ResolveProjectSlug for how it and queueRoot combine
// into the namespace folded into every label/filename this plan produces.
func ComputeDaemonPlan(goos, binaryPath, queueRoot, logDir, schedule, launchAgentsDir, projectID string) DaemonPlan {
	if goos != "darwin" {
		return DaemonPlan{Supported: false, SkipNote: fmt.Sprintf("daemon install not yet supported on %s — TODO systemd", goos)}
	}

	// Guard against a dead daemon: the waker/sweep both take --queue <queueRoot>, so a
	// plist pointing at a queue dir that doesn't exist is a service that can never do
	// useful work (the incident: running init from a repo whose profile's ../queue
	// resolved to an absent ~/Projects/queue wrote dev.leadkit.{waker,sweep}.projects
	// aimed at nothing). Skip the whole daemon plan with a clear note — never a hard
	// error, so hooks/skills still plan and apply.
	if !daemonQueueRootExists(queueRoot) {
		return DaemonPlan{
			Supported:    false,
			QueueMissing: true,
			SkipNote: fmt.Sprintf(
				"queue root %s does not exist — skipping daemon install; run init from the "+
					"project whose queue this is, or create the dir", queueRoot),
		}
	}

	slug := ResolveProjectSlug(projectID, queueRoot)
	warning := ""
	if slug == "" {
		warning = unresolvedSlugWarning(queueRoot)
	}

	// WorkingDirectory: pin launchd's cwd (which defaults to `/`) to the project root
	// so any cwd-relative resolution in the waker/sweep is correct — defense in depth
	// behind `queue sweep`'s own `--queue`-derived artifact/re-anchor scoping. Discover
	// the project by walking up from the queue root's parent (the same walk config uses
	// from cwd); an unresolvable queue (no leadkit.yaml above it) falls back to the
	// queue's parent as a best-effort project-root guess (queues sit at <project>/queue).
	workingDir := discoverProjectRoot(filepath.Dir(queueRoot))

	wakerLabel := daemonLabel(WakerLabelBase, slug)
	wakerPlist := RenderPlist(PlistSpec{
		Label:             wakerLabel,
		ProgramArguments:  []string{binaryPath, "queue", "watch", "--queue", queueRoot},
		KeepAlive:         true,
		RunAtLoad:         true,
		StandardOutPath:   filepath.Join(logDir, "waker.log"),
		StandardErrorPath: filepath.Join(logDir, "waker.err.log"),
		WorkingDirectory:  workingDir,
	})
	wakerSpec := ServiceSpec{
		Label:     wakerLabel,
		PlistPath: filepath.Join(launchAgentsDir, wakerLabel+".plist"),
		Plist:     wakerPlist,
	}

	startInterval, cal, note := cronToLaunchd(schedule)
	sweepLabel := daemonLabel(SweepLabelBase, slug)
	sweepSpecInput := PlistSpec{
		Label:             sweepLabel,
		ProgramArguments:  []string{binaryPath, "queue", "sweep", "--queue", queueRoot},
		RunAtLoad:         true,
		StandardOutPath:   filepath.Join(logDir, "sweep.log"),
		StandardErrorPath: filepath.Join(logDir, "sweep.err.log"),
		WorkingDirectory:  workingDir,
	}
	if cal != nil {
		sweepSpecInput.StartCalendar = cal
	} else {
		sweepSpecInput.StartIntervalSec = startInterval
	}
	sweepSpec := ServiceSpec{
		Label:     sweepLabel,
		PlistPath: filepath.Join(launchAgentsDir, sweepLabel+".plist"),
		Plist:     RenderPlist(sweepSpecInput),
		Note:      note,
	}

	var services []ServiceStatus
	for _, spec := range []ServiceSpec{wakerSpec, sweepSpec} {
		changed := true
		if existing, err := os.ReadFile(spec.PlistPath); err == nil && bytes.Equal(existing, spec.Plist) {
			changed = false
		}
		services = append(services, ServiceStatus{Spec: spec, Changed: changed})
	}
	return DaemonPlan{Supported: true, Services: services, Slug: slug, Warning: warning}
}

// ComputeBurnDaemonPlan is the account-level burn-enforce daemon's part of `leadkit
// init`'s plan (docs/build-tasks/burn-auto-enforce.md) — SEPARATE from ComputeDaemonPlan
// because it is account-scoped, not per-project: it takes no queue root, isn't skipped
// when this project's queue is absent, and is never slugged (see BurnLabel). Pure except
// for the "does this exact content already exist" read (never a write). goos is passed
// in (not read from runtime.GOOS) so it's testable on any host. binaryPath MUST be
// absolute (launchd doesn't reliably see a shell's PATH) — see ResolveBinaryPath.
// workingDir is launchd's cwd for the job (the account leadkit dir, e.g.
// ~/.claude/leadkit); intervalSec is the tick cadence (see burn.DefaultEnforceInterval).
func ComputeBurnDaemonPlan(goos, binaryPath, logDir, workingDir, launchAgentsDir string, intervalSec int) DaemonPlan {
	if goos != "darwin" {
		return DaemonPlan{Supported: false, SkipNote: fmt.Sprintf("burn-enforce daemon install not yet supported on %s — TODO systemd", goos)}
	}
	if intervalSec <= 0 {
		intervalSec = int(60 * 2) // defensive: never emit a zero/negative StartInterval
	}
	spec := ServiceSpec{
		Label:     BurnLabel,
		PlistPath: filepath.Join(launchAgentsDir, BurnLabel+".plist"),
		Plist: RenderPlist(PlistSpec{
			Label:             BurnLabel,
			ProgramArguments:  []string{binaryPath, "burn", "enforce"},
			RunAtLoad:         true,
			StandardOutPath:   filepath.Join(logDir, "burn-enforce.log"),
			StandardErrorPath: filepath.Join(logDir, "burn-enforce.err.log"),
			WorkingDirectory:  workingDir,
			StartIntervalSec:  intervalSec,
		}),
	}
	changed := true
	if existing, err := os.ReadFile(spec.PlistPath); err == nil && bytes.Equal(existing, spec.Plist) {
		changed = false
	}
	return DaemonPlan{Supported: true, Services: []ServiceStatus{{Spec: spec, Changed: changed}}}
}

// FormatDaemonPlan renders plan for `leadkit init`'s output — the plist content that
// would be (or was) written for every changed service, and a one-liner for anything
// already installed unchanged. Printed for BOTH dry-run and a real apply (the exact
// plan is what a real run acts on).
func FormatDaemonPlan(plan DaemonPlan) string {
	var b strings.Builder
	if !plan.Supported {
		fmt.Fprintf(&b, "daemon: %s\n", plan.SkipNote)
		return b.String()
	}
	if plan.Slug != "" {
		fmt.Fprintf(&b, "daemon: project slug %q — labels namespaced, safe to coexist with other projects\n", plan.Slug)
	}
	if plan.Warning != "" {
		fmt.Fprintf(&b, "WARNING: %s\n", plan.Warning)
	}
	for _, s := range plan.Services {
		if s.Changed {
			fmt.Fprintf(&b, "+ %s -> %s\n", s.Spec.Label, s.Spec.PlistPath)
			b.Write(s.Spec.Plist)
		} else {
			fmt.Fprintf(&b, "= %s already installed, unchanged: %s\n", s.Spec.Label, s.Spec.PlistPath)
		}
		if s.Spec.Note != "" {
			fmt.Fprintf(&b, "  note: %s\n", s.Spec.Note)
		}
	}
	return b.String()
}

// runLaunchctl execs `launchctl <args...>`, returning combined output. Injectable so
// tests never actually load/unload a real service.
var runLaunchctl = func(args ...string) (output string, err error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

// loadPlist loads plistPath, trying the modern `launchctl bootstrap gui/<uid>
// <plist>` first and falling back to the legacy `launchctl load <plist>` — "detect
// and use the working one" per the spec. Returns which method worked.
func loadPlist(plistPath string) (method, output string, err error) {
	uidArg := fmt.Sprintf("gui/%d", os.Getuid())
	if out, err := runLaunchctl("bootstrap", uidArg, plistPath); err == nil {
		return "bootstrap", out, nil
	} else if out2, err2 := runLaunchctl("load", plistPath); err2 == nil {
		return "load", out2, nil
	} else {
		return "", strings.TrimSpace(out) + " / " + strings.TrimSpace(out2),
			fmt.Errorf("launchctl bootstrap failed (%v); launchctl load also failed (%v)", err, err2)
	}
}

// DaemonApplyResult is one service's outcome from ApplyDaemonPlan.
type DaemonApplyResult struct {
	Label      string
	Skipped    bool // Changed was false — already installed, left exactly alone
	BackupPath string
	LoadMethod string // "bootstrap" | "load" | "" (Skipped, or LoadErr != nil)
	LoadErr    error
}

// ApplyDaemonPlan writes every Changed service's plist (atomically, backing up
// whatever it replaces) and — when loadServices is true — loads it. An unchanged
// service is left exactly alone — idempotent by construction, same as the hooks
// side: a second run over an already-installed, byte-identical plist finds nothing
// to do and never touches launchctl for it at all.
//
// loadServices=false writes the plist(s) but never calls launchctl at all (LoadMethod
// is reported "skipped"). This exists for exactly one reason: a subprocess test that
// execs the REAL built binary cannot inject the runLaunchctl stub (that only affects
// THIS process's memory, not a separately-exec'd one) — sandboxing HOME/cwd/profile
// keeps the plist CONTENT harmless, but the launchctl call itself is still a real
// registration with the host's actual launchd session regardless of what the plist
// points at. cmd/leadkit's `init --skip-launchctl` flag is the only caller that should
// ever pass false, and only from tests — see docs/build-tasks/init-daemon-cron.md's
// post-incident follow-up (a subprocess test installed real LaunchAgents once already).
func ApplyDaemonPlan(plan DaemonPlan, launchAgentsDir string, loadServices bool) ([]DaemonApplyResult, error) {
	if !plan.Supported {
		return nil, nil
	}
	if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
		return nil, fmt.Errorf("install: mkdir %s: %w", launchAgentsDir, err)
	}

	var results []DaemonApplyResult
	for _, svc := range plan.Services {
		if !svc.Changed {
			results = append(results, DaemonApplyResult{Label: svc.Spec.Label, Skipped: true})
			continue
		}

		res := DaemonApplyResult{Label: svc.Spec.Label}
		if existing, err := os.ReadFile(svc.Spec.PlistPath); err == nil {
			backup := fmt.Sprintf("%s.bak-%s", svc.Spec.PlistPath, time.Now().UTC().Format("20060102T150405Z"))
			if err := os.WriteFile(backup, existing, 0o644); err != nil {
				return results, fmt.Errorf("install: backup %s: %w", backup, err)
			}
			res.BackupPath = backup
		} else if !os.IsNotExist(err) {
			return results, fmt.Errorf("install: read %s: %w", svc.Spec.PlistPath, err)
		}

		tmp, err := os.CreateTemp(launchAgentsDir, ".leadkit-plist-*.tmp")
		if err != nil {
			return results, fmt.Errorf("install: create temp plist: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(svc.Spec.Plist); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return results, fmt.Errorf("install: write temp plist: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpPath)
			return results, fmt.Errorf("install: close temp plist: %w", err)
		}
		if err := os.Rename(tmpPath, svc.Spec.PlistPath); err != nil {
			os.Remove(tmpPath)
			return results, fmt.Errorf("install: rename plist into place: %w", err)
		}

		if !loadServices {
			res.LoadMethod = "skipped"
			results = append(results, res)
			continue
		}
		method, _, loadErr := loadPlist(svc.Spec.PlistPath)
		res.LoadMethod = method
		res.LoadErr = loadErr
		results = append(results, res)
	}
	return results, nil
}

// ResolveBinaryPath finds the leadkit binary's absolute path for a launchd
// ProgramArguments entry — launchd does not reliably see an interactive shell's
// PATH, so the hooks part's bare "leadkit" isn't good enough here. Tries $PATH first
// (the normal `go install`/`make install` location), falling back to the currently
// running executable's own path (e.g. a dev `go run` invocation).
func ResolveBinaryPath() string {
	if p, err := exec.LookPath("leadkit"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "leadkit"
}
