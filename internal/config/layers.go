// Layered config (docs/PORTABLE-OPERATING-KIT.md): leadkit's effective Profile is
// resolved from up to three merged layers — user, team, project — git-style. This
// file is the resolution engine; config.go's Load()/Defaults()/Profile stay the
// single place every consumer reads from (LoadWithOrigin is the ONLY new entry
// point, added for `config show --show-origin`; Load()'s own signature/behavior is
// unchanged for every existing caller).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// userConfigRelPath is the user layer's path under XDG_CONFIG_HOME (or ~/.config
// when unset).
const userConfigRelPath = "leadkit/config.yaml"

// KeyOrigin is one resolved top-level key's provenance — which layer supplied its
// effective value, and whether the team locked it. A key absent from every
// layer's KeyOrigin entry came from Defaults() (`--show-origin` reports it as
// "default").
type KeyOrigin struct {
	Layer  string // "user" | "team" | "project" | "default"
	Locked bool
}

// discoverUserConfigPath returns the user layer's config path: $XDG_CONFIG_HOME/
// leadkit/config.yaml if it exists, else ~/.config/leadkit/config.yaml, else the
// legacy ~/.leadkit/config.yaml, else "" (no user layer — fine, backward-compat).
func discoverUserConfigPath() string {
	home := os.Getenv("HOME")
	xdgBase := os.Getenv("XDG_CONFIG_HOME")
	if xdgBase == "" && home != "" {
		xdgBase = filepath.Join(home, ".config")
	}
	if xdgBase != "" {
		if p := filepath.Join(xdgBase, userConfigRelPath); fileIsReadable(p) {
			return p
		}
	}
	if home != "" {
		if p := filepath.Join(home, ".leadkit", "config.yaml"); fileIsReadable(p) {
			return p
		}
	}
	return ""
}

func fileIsReadable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// rawTopLevelKeys parses data's TOP-LEVEL YAML mapping keys into raw yaml.Nodes —
// used only to know WHICH keys a layer actually set (for --show-origin's own
// top-level-only annotation), never as the decoded value itself (unmarshaling
// onto Profile handles that). Returns nil for anything that isn't a top-level
// mapping (empty file, scalar document, ...) rather than erroring — treated the
// same as "sets nothing".
func rawTopLevelKeys(data []byte) map[string]yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	out := map[string]yaml.Node{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		out[mapping.Content[i].Value] = *mapping.Content[i+1]
	}
	return out
}

// rawKeyPaths recursively parses data's YAML into DOTTED key paths (e.g.
// "burn.session_reserve"), for every key reachable from the top-level mapping —
// the top-level keys themselves are included too (a 1-segment "path"). Lets a
// PreferenceKeys/locked entry name a NESTED field (burn splits: session_reserve is
// personal, op_rate_warn/agent_rate_warn stay team policy) without needing a
// separate mechanism from the top-level case. Only descends into mapping nodes —
// every key this actually needs (burn.*, ...) is nested under a plain mapping,
// matching Profile's own shape.
func rawKeyPaths(data []byte) map[string]yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	out := map[string]yaml.Node{}
	collectKeyPaths("", doc.Content[0], out)
	return out
}

func collectKeyPaths(prefix string, node *yaml.Node, out map[string]yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i].Value
		v := node.Content[i+1]
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		out[path] = *v
		collectKeyPaths(path, v, out)
	}
}

// lockedKeys reads teamKeys' own `locked: [...]` entry, if any.
func lockedKeys(teamKeys map[string]yaml.Node) []string {
	node, ok := teamKeys["locked"]
	if !ok {
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return nil
	}
	return list
}

// applyRawKeyPath overlays ONLY path's value onto merged — path may be a
// top-level key ("outputs") or a dotted nested one ("burn.session_reserve"); a
// synthetic single-path YAML document is built either way, so unmarshaling it
// touches exactly that one field and leaves every other already-resolved field
// alone. Used by the preference/lock correction passes, which need to override a
// single key without re-running (and re-clobbering) the generic layer overlay.
func applyRawKeyPath(merged *Profile, path string, value yaml.Node) error {
	parts := strings.Split(path, ".")
	node := &value
	for i := len(parts) - 1; i > 0; i-- {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: parts[i]}
		node = &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{keyNode, node}}
	}
	topKeyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: parts[0]}
	doc := yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{topKeyNode, node}}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("config: re-marshal key %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, merged); err != nil {
		return fmt.Errorf("config: apply key %q: %w", path, err)
	}
	return nil
}

// resolveLayers is the whole precedence engine. userData/teamData/projectData are
// each that layer's raw file bytes (nil = layer absent). projectRoot seeds
// Profile.root exactly as loadFile does today.
//
// Precedence, per key (see docs/build-tasks/layered-config.md, the design's own
// wording):
//  1. If the TEAM config locks the key -> the team's value wins over everything.
//  2. Else if the key is a PREFERENCE key (PreferenceKeys) -> user > project > team.
//  3. Else (a normal key) -> project > team > user.
//
// Implementation: pass 1 overlays user, then team, then project (in that
// ASCENDING-priority order) onto Defaults() — since a later layer only touches
// the keys IT sets, whichever ran last wins per key, which is exactly rule 3 for
// every key (nested struct fields merge the same way yaml.Unmarshal always has —
// e.g. a layer setting only burn.op_rate_warn never clobbers burn.session_reserve
// from an earlier layer). Pass 2 corrects PreferenceKeys (which may name a nested
// dotted path, e.g. "burn.session_reserve"): if the user layer set one, it always
// wins over whatever pass 1 computed (project/team, if either set it) — if the
// user layer did NOT set it, pass 1's project>team result is already exactly
// right (preference falls back to project>team when the user expresses no
// preference). Pass 3 corrects locked keys: the team's own value wins outright,
// overriding both prior passes — applied last so it always has the final say.
func resolveLayers(userData, teamData, projectData []byte, projectRoot string) (*Profile, map[string]KeyOrigin, error) {
	userTop := rawTopLevelKeys(userData)
	teamTop := rawTopLevelKeys(teamData)
	projectTop := rawTopLevelKeys(projectData)
	// Dotted-path views of the same three files — pass 2/3 need to see INTO
	// nested blocks (burn.*, ...), pass 1's origin tracking stays top-level-only
	// (what --show-origin actually renders).
	userPaths := rawKeyPaths(userData)
	teamPaths := rawKeyPaths(teamData)

	merged := Defaults()
	origins := map[string]KeyOrigin{}

	overlay := func(data []byte, keys map[string]yaml.Node, layer string) error {
		if data == nil {
			return nil
		}
		if err := yaml.Unmarshal(data, &merged); err != nil {
			return fmt.Errorf("config: parse %s config: %w", layer, err)
		}
		for k := range keys {
			origins[k] = KeyOrigin{Layer: layer}
		}
		return nil
	}
	if err := overlay(userData, userTop, "user"); err != nil {
		return nil, nil, err
	}
	if err := overlay(teamData, teamTop, "team"); err != nil {
		return nil, nil, err
	}
	if err := overlay(projectData, projectTop, "project"); err != nil {
		return nil, nil, err
	}

	// Pass 2: preference keys — user wins outright when the user layer sets one
	// (top-level or a dotted nested path, e.g. "burn.session_reserve").
	for key := range PreferenceKeys {
		if node, ok := userPaths[key]; ok {
			if err := applyRawKeyPath(&merged, key, node); err != nil {
				return nil, nil, err
			}
			origins[key] = KeyOrigin{Layer: "user"}
		}
	}

	// Pass 3: team-locked keys — the team's value wins outright, always last.
	for _, key := range lockedKeys(teamTop) {
		o := origins[key]
		o.Locked = true
		if node, ok := teamPaths[key]; ok {
			if err := applyRawKeyPath(&merged, key, node); err != nil {
				return nil, nil, err
			}
			o.Layer = "team"
		}
		origins[key] = o
	}

	merged.root = projectRoot
	return &merged, origins, nil
}

// loadLayerFile reads path, tolerating a missing file (returns nil, nil — "layer
// absent", never an error) but surfacing a genuine read/permission failure.
func loadLayerFile(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// resolveTeamConfigPath reads projectData's own `team_config:` key (if any) and
// resolves it the same way Profile.Resolve() would (project-root-relative, ~/,
// $ENV) — done by a temporary Profile rather than the final merged one, since the
// team layer must be read BEFORE the final merge exists.
func resolveTeamConfigPath(projectData []byte, projectRoot string) string {
	if projectData == nil {
		return ""
	}
	tmp := Defaults()
	if err := yaml.Unmarshal(projectData, &tmp); err != nil {
		return ""
	}
	if tmp.TeamConfig == "" {
		return ""
	}
	tmp.root = projectRoot
	return tmp.Resolve(tmp.TeamConfig)
}

// loadLayered is Load()/LoadWithOrigin's shared implementation: discovers +
// reads all three layers and resolves them. projectPath == "" means no project
// leadkit.yaml was found (Defaults()-only, exactly today's behavior) — the user
// layer is still consulted (it's independent of the project), the team layer
// is not (team_config only ever comes from the project layer).
func loadLayered(projectPath string) (*Profile, map[string]KeyOrigin, error) {
	var projectData []byte
	projectRoot := ""
	if projectPath != "" {
		data, err := os.ReadFile(projectPath)
		if err != nil {
			return nil, nil, fmt.Errorf("config: read %s: %w", projectPath, err)
		}
		projectData = data
		projectRoot = filepath.Dir(projectPath)
	}

	userData, err := loadLayerFile(discoverUserConfigPath())
	if err != nil {
		return nil, nil, fmt.Errorf("config: read user config: %w", err)
	}

	teamPath := resolveTeamConfigPath(projectData, projectRoot)
	teamData, err := loadLayerFile(teamPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: read team config (%s): %w", teamPath, err)
	}

	return resolveLayers(userData, teamData, projectData, projectRoot)
}
