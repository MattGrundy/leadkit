// Package hierarchy is `leadkit hierarchy show|propose` plus the uniform `get`/
// `list`/`tree` read helpers — read-only, zero-impact. It speaks the locked taxonomy
// (docs/HIERARCHY-AND-SYNC.md): Project (the goal) -> Milestone -> Epic (all three in
// milestones.yaml) -> Issue (issues/*.md, `parent:` = epic id) -> Task (tasks/*.md,
// `parent:` = issue id — the current ~1400 tasks, unrenamed). Rollup flows through
// BOTH file levels: Task terminal-counts roll into its Issue, Issue into its Epic,
// Epic into its Milestone. Nothing here writes anything — see propose.go's own doc
// comment for why "propose" stops at a suggestion, never an assignment.
package hierarchy

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Task is one epic's own nested breakdown item (the "quarantine" epic's tasks: list
// is the only example in the seed milestones.yaml, but the shape is general) — a
// bare string id, or a map with a note. Same polymorphism as Epic below.
type Task struct {
	ID   string `yaml:"id"`
	Note string `yaml:"note,omitempty"`
}

// UnmarshalYAML accepts either a bare scalar id or a {id, note} mapping — milestones.yaml
// uses both shapes interchangeably (a bare `epics:` id list vs `epics`/`tasks:` mapping
// entries).
func (t *Task) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var id string
		if err := value.Decode(&id); err != nil {
			return err
		}
		*t = Task{ID: id}
		return nil
	}
	type rawTask Task
	var r rawTask
	if err := value.Decode(&r); err != nil {
		return err
	}
	*t = Task(r)
	return nil
}

// Epic is one milestone's epic entry — a bare string id, or a map carrying state/
// note/desc/acceptance/depends_on, optionally with its own nested Tasks (the
// "quarantine" epic in the seed file is the one example of this richer shape).
type Epic struct {
	ID         string   `yaml:"id"`
	State      string   `yaml:"state,omitempty"`
	Note       string   `yaml:"note,omitempty"`
	Desc       string   `yaml:"desc,omitempty"`
	Acceptance string   `yaml:"acceptance,omitempty"`
	DependsOn  []string `yaml:"depends_on,omitempty"`
	Tasks      []Task   `yaml:"tasks,omitempty"`
	// Aliases are extra subject-keyword strings propose/cluster-issues match a
	// Task's title against, alongside the epic's own ID and Title — for when
	// neither reads the way people actually phrase the work (e.g. epic
	// "spike-20608" only reads that way in the yaml; the tasks say "PR #18883").
	// Optional; absent = today's id/title-only matching, unaffected.
	Aliases []string `yaml:"aliases,omitempty"`
}

// UnmarshalYAML mirrors Task's — see its doc comment.
func (e *Epic) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var id string
		if err := value.Decode(&id); err != nil {
			return err
		}
		*e = Epic{ID: id}
		return nil
	}
	type rawEpic Epic
	var r rawEpic
	if err := value.Decode(&r); err != nil {
		return err
	}
	*e = Epic(r)
	return nil
}

// Milestone is one goal-level milestone; Epics is optional (a milestone can be just
// a state marker with no breakdown yet, e.g. "poc"/"green-run" in the seed file).
type Milestone struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	State string `yaml:"state"`
	Desc  string `yaml:"desc,omitempty"`
	Epics []Epic `yaml:"epics,omitempty"`
}

// Model is the whole parsed milestones.yaml — Level-1 of the locked taxonomy is
// Project (Project -> Milestone -> Epic -> Issue -> Task).
type Model struct {
	Project    string
	Milestones []Milestone
}

// UnmarshalYAML reads the top-level `project:` field, falling back to the
// deprecated `goal:` alias (milestones.yaml's original field name, pre-taxonomy-
// lock) so an old file keeps parsing unchanged. `project:` wins if a file somehow
// has both.
func (m *Model) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Project    string      `yaml:"project"`
		Goal       string      `yaml:"goal"` // deprecated alias for Project
		Milestones []Milestone `yaml:"milestones"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	project := raw.Project
	if project == "" {
		project = raw.Goal
	}
	*m = Model{Project: project, Milestones: raw.Milestones}
	return nil
}

// Parse reads and parses path (the store-root milestones.yaml). A missing file
// returns Model{}, false, nil — NOT an error: hierarchy commands must degrade
// gracefully before the hierarchy model is seeded (matching every other "optional
// feature, absence is normal" convention in this codebase).
func Parse(path string) (Model, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Model{}, false, nil
		}
		return Model{}, false, err
	}
	var m Model
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Model{}, true, err
	}
	return m, true, nil
}

// KnownIDs returns every epic id + nested task id in model — a Task's valid
// `parent:` targets, PRE-Issue-level (a Task parented directly to an epic, or to
// one of an epic's own legacy nested YAML breakdown items). ComputeRollup adds every
// real Issue id to this set too, since Issue is now a Task's canonical parent.
func (m Model) KnownIDs() map[string]bool {
	out := map[string]bool{}
	for _, ms := range m.Milestones {
		for _, e := range ms.Epics {
			out[e.ID] = true
			for _, t := range e.Tasks {
				out[t.ID] = true
			}
		}
	}
	return out
}

// EpicIDs returns just the top-level epic ids in model — narrower than KnownIDs
// (which also includes an epic's own nested legacy breakdown items). This is the
// valid `parent:` target set for an Issue (taxonomy: Epic -> Issue), and for
// `issue create --epic`'s validation.
func (m Model) EpicIDs() map[string]bool {
	out := map[string]bool{}
	for _, ms := range m.Milestones {
		for _, e := range ms.Epics {
			out[e.ID] = true
		}
	}
	return out
}

// MilestoneIDs returns every milestone id in model.
func (m Model) MilestoneIDs() map[string]bool {
	out := map[string]bool{}
	for _, ms := range m.Milestones {
		out[ms.ID] = true
	}
	return out
}

// terminalStatuses mirrors every other internal package's own copy (see e.g.
// internal/orchestrate's build.go) — duplicated on purpose, this codebase's
// small-package decoupling convention.
var terminalStatuses = map[string]bool{
	"completed": true, "done": true, "dropped": true, "superseded": true,
}

func normalizeStatus(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}
