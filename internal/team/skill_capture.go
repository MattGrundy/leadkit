package team

// Skill versioning (adoption-onboard deliverable 3): Capture reads the LIVE
// leadkit-adopt skill directory (cfg.SkillsDir — normally what `leadkit init
// --apply` wrote from internal/onboard.SkillMD) and snapshots it into
// <TeamDir>/skills/<dir-name>/, the same "read live, write snapshot" shape
// AgentsDir already follows for agent defs. Unlike AgentsDir, a missing
// SkillsDir is NOT an error: `init --apply` may simply not have run yet on this
// project, and that's a perfectly normal state for `team capture` to see.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// captureSkill snapshots cfg.SkillsDir's files into
// <cfg.TeamDir>/skills/<base(cfg.SkillsDir)>/. dirName is "" when there was
// nothing to capture (cfg.SkillsDir unset, or not present on disk yet) —
// best-effort, never an error in that case.
func captureSkill(cfg Config) (files []CapturedFile, dirName string, err error) {
	if cfg.SkillsDir == "" {
		return nil, "", nil
	}
	entries, rerr := os.ReadDir(cfg.SkillsDir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("team: read skills_dir %s: %w", cfg.SkillsDir, rerr)
	}

	dirName = filepath.Base(cfg.SkillsDir)
	outDir := filepath.Join(cfg.TeamDir, "skills", dirName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("team: mkdir %s: %w", outDir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		data, rerr := os.ReadFile(filepath.Join(cfg.SkillsDir, name))
		if rerr != nil {
			return nil, "", fmt.Errorf("team: read %s: %w", name, rerr)
		}
		if err := os.WriteFile(filepath.Join(outDir, name), data, 0o644); err != nil {
			return nil, "", fmt.Errorf("team: write %s: %w", name, err)
		}
		files = append(files, CapturedFile{Name: name, Size: int64(len(data))})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, dirName, nil
}
