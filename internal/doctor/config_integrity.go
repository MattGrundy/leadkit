package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
)

// configHistoryDir is where external-config snapshots are kept: beside the config directory,
// not in it, so a change to the config does not also move its own history. A var so tests do not
// touch the real account.
var configHistoryDir = func() string {
	return filepath.Join(filepath.Dir(config.ExternalConfigDir()), "config-history")
}

// externalConfigIntegrityCheck reports the merge authority the active external config grants and
// whether the file changed since doctor last looked (ticket 138). The file that grants merge
// authority lives outside the repo, so the two-layer review gate never sees an edit to it; this
// is the only trace one leaves.
//
// It records as well as reports: each doctor run that finds a content hash it has not seen saves
// a copy as <history>/<name>/<UTC time>-<hash12>.yaml, so the first run after an edit warns once
// (old hash, new hash, where the copy is) and the following runs are quiet. Best-effort: a history
// directory that cannot be written is reported, never fatal. A profile that is not the external
// config (in-repo, $LEADKIT_PROFILE, defaults) is not this check's business.
func externalConfigIntegrityCheck(prof *config.Profile, profilePath string, now time.Time) Check {
	const name = "config-integrity"
	if profilePath == "" || filepath.Dir(profilePath) != filepath.Clean(config.ExternalConfigDir()) {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "the profile is not an external account config — nothing to check"}
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return Check{Name: name, OK: false, Severity: SeverityWarn, Detail: "cannot read " + profilePath + " to hash it: " + err.Error()}
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	grants := mergeGrantSummary(prof)

	dir := filepath.Join(configHistoryDir(), strings.TrimSuffix(filepath.Base(profilePath), filepath.Ext(profilePath)))
	prevHash, prevAt := latestSnapshot(dir)
	record := func() error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		snap := filepath.Join(dir, now.UTC().Format("20060102T150405Z")+"-"+short(hash)+".yaml")
		return os.WriteFile(snap, data, 0o600)
	}

	switch {
	case prevHash == "":
		if err := record(); err != nil {
			return Check{Name: name, OK: false, Severity: SeverityWarn, Detail: fmt.Sprintf("%s; could not record a baseline in %s: %v", grants, dir, err)}
		}
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("%s; first look at this file, baseline %s saved in %s", grants, short(hash), dir)}
	case strings.HasPrefix(hash, prevHash):
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("%s; unchanged since %s (%s)", grants, prevAt.Format(time.RFC3339), short(hash))}
	}
	if err := record(); err != nil {
		return Check{Name: name, OK: false, Severity: SeverityWarn, Detail: fmt.Sprintf("%s; CHANGED since %s (%s -> %s) and the new copy could not be saved in %s: %v", grants, prevAt.Format(time.RFC3339), short(prevHash), short(hash), dir, err)}
	}
	return Check{Name: name, OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%s; this file CHANGED since doctor last saw it at %s (%s -> %s). Copies are in %s: diff them. If you did not make the change, treat the grants above as untrusted.", grants, prevAt.Format(time.RFC3339), short(prevHash), short(hash), dir)}
}

// latestSnapshot is the hash and time of the newest snapshot in dir ("" if none). Names sort by
// time because they start with a fixed-width UTC stamp.
func latestSnapshot(dir string) (hash string, at time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", time.Time{}
	}
	sort.Strings(names)
	last := names[len(names)-1]
	stamp, rest, ok := strings.Cut(strings.TrimSuffix(last, ".yaml"), "-")
	if !ok {
		return "", time.Time{}
	}
	at, _ = time.Parse("20060102T150405Z", stamp)
	// The name carries only a short hash; the full digest is recomputed from the copy when it can
	// be read, so the comparison is on all of it.
	if data, err := os.ReadFile(filepath.Join(dir, last)); err == nil {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]), at
	}
	return rest, at
}

// short is the first 12 hex digits of a digest, for names and messages.
func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// mergeGrantSummary names every authority entry that can merge or carries a merge grant, or says
// that none does.
func mergeGrantSummary(prof *config.Profile) string {
	var parts []string
	for repo, spec := range prof.Authority {
		if !spec.CanMerge.Allow && !spec.CanMerge.Human && spec.MergeGrant == "" {
			continue
		}
		can := "false"
		switch {
		case spec.CanMerge.Human:
			can = "human"
		case spec.CanMerge.Allow:
			can = "true"
		}
		p := "authority." + repo + ": can_merge=" + can
		if spec.MergeGrant != "" {
			p += " merge_grant=" + spec.MergeGrant
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no authority entry grants merge"
	}
	sort.Strings(parts)
	return "merge authority granted: " + strings.Join(parts, "; ")
}
