package install

// Skill install (adoption-onboard deliverable 3's wiring) — `leadkit init` drops
// the leadkit-adopt skill into ~/.claude/skills/<name>/ from its EMBEDDED content
// (internal/onboard.SkillMD), same dry-run-default / --apply --confirm guard as
// the hooks/daemon plans in this package. Additive + non-clobbering by
// construction: every write stays inside skillsDir/skillName/ — a sibling skill
// dir is never read, listed, or touched.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SkillFile is one embedded skill file ComputeSkillPlan would (or
// ApplySkillPlan did) write.
type SkillFile struct {
	Name     string // relative name within the skill dir, e.g. "SKILL.md"
	DestPath string
	Content  []byte
	Changed  bool // false: DestPath already has this exact content
}

// SkillPlan is `leadkit init`'s skill-install plan for ONE named skill —
// computed by ComputeSkillPlan, executed by ApplySkillPlan.
type SkillPlan struct {
	Dir   string // skillsDir/skillName
	Files []SkillFile
}

// AnyChanged reports whether Apply would write anything.
func (p SkillPlan) AnyChanged() bool {
	for _, f := range p.Files {
		if f.Changed {
			return true
		}
	}
	return false
}

// ComputeSkillPlan computes what installing skillName's files (name -> embedded
// content) into skillsDir/skillName/ would do — read-only (stats/reads only the
// destination files to detect whether they already match), writes nothing.
// files is normally a single {"SKILL.md": onboard.SkillMD} entry today, but takes
// a map so a skill gaining a second file later needs no signature change.
func ComputeSkillPlan(skillsDir, skillName string, files map[string][]byte) SkillPlan {
	dir := filepath.Join(skillsDir, skillName)
	plan := SkillPlan{Dir: dir}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		dest := filepath.Join(dir, name)
		content := files[name]
		plan.Files = append(plan.Files, SkillFile{
			Name: name, DestPath: dest, Content: content, Changed: !sameSkillContent(dest, content),
		})
	}
	return plan
}

func sameSkillContent(path string, want []byte) bool {
	existing, err := os.ReadFile(path)
	return err == nil && bytes.Equal(existing, want)
}

// FormatSkillPlan renders plan for `leadkit init`'s output — printed for BOTH
// dry-run and a real apply, same convention as install.FormatPlan/FormatDaemonPlan.
func FormatSkillPlan(plan SkillPlan) string {
	var b bytes.Buffer
	any := false
	for _, f := range plan.Files {
		if f.Changed {
			any = true
			fmt.Fprintf(&b, "+ skill %s -> %s\n", f.Name, f.DestPath)
		}
	}
	if !any {
		fmt.Fprintf(&b, "skill: nothing to add — %s already matches\n", plan.Dir)
	}
	return b.String()
}

// SkillApplyResult is what ApplySkillPlan actually wrote.
type SkillApplyResult struct {
	WrittenFiles []string          // names actually written
	BackedUpTo   map[string]string // name -> backup path, for anything overwritten
}

// ApplySkillPlan performs plan's writes: every Changed file is written
// atomically (temp file + rename), backing up whatever it replaces first — same
// shape as team.ApplyInstallPlan's writeItem. Never touches any path outside
// plan.Dir.
func ApplySkillPlan(plan SkillPlan) (SkillApplyResult, error) {
	res := SkillApplyResult{BackedUpTo: map[string]string{}}

	for _, f := range plan.Files {
		if !f.Changed {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.DestPath), 0o755); err != nil {
			return res, fmt.Errorf("install: mkdir %s: %w", filepath.Dir(f.DestPath), err)
		}
		if existing, err := os.ReadFile(f.DestPath); err == nil {
			backup := fmt.Sprintf("%s.bak-%s", f.DestPath, time.Now().UTC().Format("20060102T150405Z"))
			if err := os.WriteFile(backup, existing, 0o644); err != nil {
				return res, fmt.Errorf("install: backup %s: %w", backup, err)
			}
			res.BackedUpTo[f.Name] = backup
		} else if !os.IsNotExist(err) {
			return res, fmt.Errorf("install: read %s: %w", f.DestPath, err)
		}

		dir := filepath.Dir(f.DestPath)
		tmp, err := os.CreateTemp(dir, ".leadkit-skill-*.tmp")
		if err != nil {
			return res, fmt.Errorf("install: create temp file: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(f.Content); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return res, fmt.Errorf("install: write temp file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpPath)
			return res, fmt.Errorf("install: close temp file: %w", err)
		}
		if err := os.Rename(tmpPath, f.DestPath); err != nil {
			os.Remove(tmpPath)
			return res, fmt.Errorf("install: rename into place: %w", err)
		}
		res.WrittenFiles = append(res.WrittenFiles, f.Name)
	}
	return res, nil
}
