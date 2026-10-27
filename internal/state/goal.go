package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Goal is the Session Goal / North Star: the durable objective a session (and every
// agent spawned from it) stays pointed at. Rarely changes.
type Goal struct {
	Title            string    `yaml:"title"`
	Objective        string    `yaml:"objective"`
	DefinitionOfDone []string  `yaml:"definition_of_done,omitempty"`
	Updated          time.Time `yaml:"updated"`
}

// DefaultGoal is the stub EnsureGoal writes when no goal file exists yet.
func DefaultGoal() Goal {
	return Goal{
		Title:     "Session Goal (stub — replace me)",
		Objective: "TODO: set the one-line north star for this project.",
		Updated:   time.Now().UTC(),
	}
}

// LoadGoal reads and parses path. The error is os.IsNotExist/errors.Is(err,
// os.ErrNotExist)-checkable when the file is simply absent.
func LoadGoal(path string) (Goal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Goal{}, err
	}
	yamlBlock, _, err := splitFrontmatter(data)
	if err != nil {
		return Goal{}, fmt.Errorf("state: goal %s: %w", path, err)
	}
	var g Goal
	if err := yaml.Unmarshal(yamlBlock, &g); err != nil {
		return Goal{}, fmt.Errorf("state: parse goal %s: %w", path, err)
	}
	return g, nil
}

// EmitGoal writes g to path as frontmatter + a rendered body, creating path's parent
// dir if needed. This is a full regenerate, not a merge — Goal is a machine-managed
// file (a SessionStart skill keeps it current), so overwriting it wholesale is the
// intended behaviour, unlike the ticket frontmatter internal/review preserves
// line-by-line.
func EmitGoal(path string, g Goal) error {
	yamlBlock, err := yaml.Marshal(g)
	if err != nil {
		return fmt.Errorf("state: marshal goal: %w", err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlBlock)
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n%s\n", g.Title, g.Objective)
	if len(g.DefinitionOfDone) > 0 {
		b.WriteString("\n## Definition of Done\n")
		for _, d := range g.DefinitionOfDone {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("state: mkdir for goal %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("state: write goal %s: %w", path, err)
	}
	return nil
}

// EnsureGoal loads path's goal if it exists, unchanged; if it doesn't, it writes
// DefaultGoal() as a stub and returns that. created reports which happened. An
// existing goal — however it reads — is NEVER overwritten by this.
func EnsureGoal(path string) (g Goal, created bool, err error) {
	g, err = LoadGoal(path)
	if err == nil {
		return g, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Goal{}, false, err
	}
	stub := DefaultGoal()
	if err := EmitGoal(path, stub); err != nil {
		return Goal{}, false, fmt.Errorf("state: write goal stub: %w", err)
	}
	return stub, true, nil
}

// FormatGoal renders g for `goal show`.
func FormatGoal(g Goal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n", g.Title, g.Objective)
	if len(g.DefinitionOfDone) > 0 {
		b.WriteString("\nDefinition of Done:\n")
		for _, d := range g.DefinitionOfDone {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	fmt.Fprintf(&b, "\nupdated: %s\n", g.Updated.Format(time.RFC3339))
	return b.String()
}
