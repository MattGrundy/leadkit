package team

// `leadkit team validate` cross-references the captured team defs against a
// (previously fetched) reference collection — READ-ONLY, advisory. It never rewrites
// anything; a finding here is a nudge for a human, never an instruction leadkit acts
// on itself. Matching (role-coverage) is by name+description token overlap, not
// exact string equality — the team's roles are named nothing like the reference's.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"mgpp.dev/leadkit/internal/starter"
)

// Finding categories.
const (
	CategoryHygiene       = "hygiene"
	CategoryToolScoping   = "tool-scoping"
	CategoryQuality       = "quality"
	CategoryCoverageExtra = "coverage-extra"
)

// ValidateFinding is one advisory observation.
type ValidateFinding struct {
	Category string `json:"category"`
	Def      string `json:"def,omitempty"` // team def filename this is about; "" for a coverage gap
	Detail   string `json:"detail"`
}

// CategoryGap is one reference CATEGORY (its top-level subdir, e.g.
// "04-quality-security") the team has thin-to-no coverage of — reported at category
// granularity rather than one line per individual reference role, since a ~160-role
// collection against a ~20-role domain-specific team would otherwise produce a wall
// of low-value gap lines.
type CategoryGap struct {
	Category string   `json:"category"`
	Total    int      `json:"total"`    // reference roles in this category
	Gapped   int      `json:"gapped"`   // of those, how many have no close team match
	Examples []string `json:"examples"` // up to 5 example reference role names, unmatched
}

// ValidateResult is `team validate`'s full advisory report.
type ValidateResult struct {
	Hygiene       []ValidateFinding `json:"hygiene"`
	ToolScoping   []ValidateFinding `json:"tool_scoping"`
	Quality       []ValidateFinding `json:"quality"`
	CoverageGaps  []CategoryGap     `json:"coverage_gaps"`
	CoverageExtra []ValidateFinding `json:"coverage_extra"`
	// StarterDrift compares the working set against the definitions embedded in the
	// binary. Distinct from the coverage sections, which compare against a fetched
	// third-party collection: this one answers "what have I changed from what leadkit
	// shipped, and did I mean to?". Divergence here is EXPECTED, not a warning.
	StarterDrift []starter.DriftFinding `json:"starter_drift"`
	// InstalledDir is the directory StarterDrift was measured against: where definitions are
	// installed, not the captured snapshot.
	InstalledDir string `json:"installed_dir,omitempty"`
}

// Summary renders the one-line total `team validate` always prints.
func (r ValidateResult) Summary() string {
	n := starter.CountByStatus(r.StarterDrift)
	return fmt.Sprintf("%d hygiene finding(s), %d tool-scoping note(s), %d quality nudge(s), %d category coverage gap(s), %d team-only role(s) (informational); vs shipped starters: %d yours, %d pristine, %d not installed",
		len(r.Hygiene), len(r.ToolScoping), len(r.Quality), len(r.CoverageGaps), len(r.CoverageExtra),
		n[starter.DriftCustomised], n[starter.DriftPristine], n[starter.DriftRemoved])
}

// coverageMatchThreshold is the minimum name+description token-overlap (Jaccard)
// score for two roles to count as "the same role" — tuned loose on purpose (a false
// "matched" costs a missed nudge; a false "gap" against a 160-role collection this
// broad is just noise). See jaccard/tokenize below.
const coverageMatchThreshold = 0.10

// roleProfile is one def (team or reference), reduced to what coverage-matching and
// the report need.
type roleProfile struct {
	Name     string
	Desc     string
	File     string // relative to its own root (team agents/ or reference dir)
	Category string // reference only: the def's immediate parent dir name
	Tokens   map[string]bool
}

// Validate compares teamAgentsDir's *.md defs against referenceDir's (as fetched by
// FetchReference — a flat-or-nested tree of *.md files, SOURCE.txt ignored). Every
// source is read-only; nothing is written. A missing/empty referenceDir still runs
// hygiene/quality/tool-scoping (coverage sections just come back empty) — validate
// never hard-fails on "no reference fetched yet".
//
// Two directories are read and they answer different questions. teamAgentsDir is the team's
// captured snapshot: it is what hygiene, tool scoping, quality and coverage judge. installedAgentsDir
// is where the harness loads definitions from and where `team install` writes them, and it is the only
// thing "is this starter installed" can be asked of. The snapshot is not where definitions get installed,
// so it cannot say.
func Validate(teamAgentsDir, installedAgentsDir, referenceDir string) (ValidateResult, error) {
	var res ValidateResult
	res.InstalledDir = installedAgentsDir

	// Starter drift first, and deliberately BEFORE the read that can fail: it is the one
	// section that still says something useful when teamAgentsDir does not exist yet (every
	// starter reads as not-installed, which is the honest description of a fresh machine).
	drift, derr := starter.CompareToStarters(installedAgentsDir)
	if derr == nil {
		res.StarterDrift = drift
	}
	// A drift error is swallowed on purpose: this section is advisory, and failing the whole
	// validate run because a comparison against our own defaults could not be made would
	// trade a nudge for an outage.

	teamEntries, err := os.ReadDir(teamAgentsDir)
	if err != nil {
		return res, fmt.Errorf("team validate: read %s: %w", teamAgentsDir, err)
	}

	var teamProfiles []roleProfile
	for _, e := range teamEntries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(teamAgentsDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // best-effort
		}
		block, ok := extractFrontmatterBlock(data)
		if !ok {
			res.Hygiene = append(res.Hygiene, ValidateFinding{
				Category: CategoryHygiene, Def: name,
				Detail: "no closing frontmatter delimiter (---) — file is not parseable as a def",
			})
			continue
		}
		var fm map[string]any
		if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
			res.Hygiene = append(res.Hygiene, ValidateFinding{
				Category: CategoryHygiene, Def: name,
				Detail: fmt.Sprintf("frontmatter is not well-formed YAML: %v", err),
			})
			continue
		}

		fieldStr := func(k string) string {
			s, _ := fm[k].(string)
			return strings.TrimSpace(s)
		}
		defName := fieldStr("name")
		desc := fieldStr("description")

		if defName == "" {
			res.Hygiene = append(res.Hygiene, ValidateFinding{Category: CategoryHygiene, Def: name, Detail: "missing `name` field"})
		}
		if desc == "" {
			res.Hygiene = append(res.Hygiene, ValidateFinding{Category: CategoryHygiene, Def: name, Detail: "missing `description` field"})
		} else if isPlaceholderDescription(desc) {
			res.Hygiene = append(res.Hygiene, ValidateFinding{Category: CategoryHygiene, Def: name, Detail: fmt.Sprintf("description reads as a placeholder: %q", desc)})
		} else if isBareLabelDescription(desc) {
			res.Quality = append(res.Quality, ValidateFinding{
				Category: CategoryQuality, Def: name,
				Detail: fmt.Sprintf("description reads as a bare category label rather than a specific role + when-to-use — routing keys off this: %q", desc),
			})
		}

		toolsRaw, hasTools := fm["tools"]
		if !hasTools {
			res.ToolScoping = append(res.ToolScoping, ValidateFinding{
				Category: CategoryToolScoping, Def: name,
				Detail: "no `tools` field — implicit unrestricted access; the reference collection scopes tools explicitly per role",
			})
		} else if toolsStr, isStr := toolsRaw.(string); isStr && isBroadToolGrant(toolsStr) {
			res.ToolScoping = append(res.ToolScoping, ValidateFinding{
				Category: CategoryToolScoping, Def: name,
				Detail: fmt.Sprintf("broad tools grant (%q) — worth a look for least-privilege, though some agents legitimately need it", strings.TrimSpace(toolsStr)),
			})
		}

		if defName == "" {
			defName = strings.TrimSuffix(name, ".md")
		}
		teamProfiles = append(teamProfiles, roleProfile{Name: defName, Desc: desc, File: name, Tokens: tokenize(defName + " " + desc)})
	}

	sortFindings(res.Hygiene)
	sortFindings(res.ToolScoping)
	sortFindings(res.Quality)

	var refProfiles []roleProfile
	if referenceDir != "" {
		_ = filepath.WalkDir(referenceDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".md") || name == "SOURCE.txt" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			block, ok := extractFrontmatterBlock(data)
			if !ok {
				return nil
			}
			var fm map[string]any
			if yaml.Unmarshal([]byte(block), &fm) != nil {
				return nil
			}
			refName, _ := fm["name"].(string)
			refName = strings.TrimSpace(refName)
			if refName == "" {
				refName = strings.TrimSuffix(name, ".md")
			}
			desc, _ := fm["description"].(string)
			rel, _ := filepath.Rel(referenceDir, path)
			refProfiles = append(refProfiles, roleProfile{
				Name: refName, Desc: desc, File: rel,
				Category: filepath.Base(filepath.Dir(rel)),
				Tokens:   tokenize(refName + " " + desc),
			})
			return nil
		})
	}

	res.CoverageGaps = coverageGapsByCategory(teamProfiles, refProfiles)
	res.CoverageExtra = coverageExtra(teamProfiles, refProfiles)

	return res, nil
}

// extractFrontmatterBlock returns the text between a leading "---" line and the
// next "---" line — the raw YAML block, unparsed. ok=false: no opening/closing
// delimiter at all (not a def in the expected shape).
func extractFrontmatterBlock(data []byte) (block string, ok bool) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), true
		}
	}
	return "", false
}

// isPlaceholderDescription flags an empty-in-spirit description — present but
// trivially short or a copy-paste stub.
func isPlaceholderDescription(desc string) bool {
	d := strings.ToLower(strings.TrimSpace(desc))
	if len(d) < 8 {
		return true
	}
	for _, stub := range []string{"todo", "tbd", "description here", "fill me in", "placeholder"} {
		if d == stub || strings.Contains(d, stub) {
			return true
		}
	}
	return false
}

// isBareLabelDescription flags a description that reads as a category label (short,
// no "when to use this" signal) rather than a specific role + trigger — the
// heuristic behind hygiene check #4. Deliberately loose: this is a nudge, not a
// rule — a short-but-clear description is common and fine, this only catches the
// clearly-thin end.
func isBareLabelDescription(desc string) bool {
	d := strings.ToLower(desc)
	words := strings.Fields(d)
	if len(words) >= 12 {
		return false // long enough to very likely carry real when-to-use content
	}
	for _, signal := range []string{"use ", "when ", "for ", " - ", " — ", ": "} {
		if strings.Contains(d, signal) {
			return false
		}
	}
	return true
}

// isBroadToolGrant flags "All tools" / "*" / an effectively-empty declaration.
func isBroadToolGrant(tools string) bool {
	t := strings.TrimSpace(tools)
	tl := strings.ToLower(t)
	return t == "" || t == "*" || tl == "all tools" || tl == "all"
}

// tokenize lowercases s, keeps letters/digits as word characters, drops short/stop
// words, and returns the remaining word set — the unit jaccard compares.
func tokenize(s string) map[string]bool {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	stop := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true, "for": true, "to": true,
		"of": true, "in": true, "on": true, "with": true, "this": true, "that": true, "agent": true,
		"use": true, "used": true, "using": true, "when": true, "is": true, "are": true, "you": true,
		"your": true, "their": true, "its": true, "into": true, "from": true, "by": true, "as": true,
		"be": true, "will": true, "should": true, "not": true,
	}
	out := map[string]bool{}
	for _, w := range strings.Fields(b.String()) {
		if len(w) < 3 || stop[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// jaccard is |A∩B| / |A∪B| — 0 when either set is empty.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// bestScore returns target's highest jaccard score against any profile in against.
func bestScore(target roleProfile, against []roleProfile) float64 {
	best := 0.0
	for _, c := range against {
		if s := jaccard(target.Tokens, c.Tokens); s > best {
			best = s
		}
	}
	return best
}

// coverageGapsByCategory groups reference profiles by Category and reports, per
// category, how many have no close team match.
func coverageGapsByCategory(teamProfiles, refProfiles []roleProfile) []CategoryGap {
	byCat := map[string][]roleProfile{}
	var cats []string
	for _, r := range refProfiles {
		if _, seen := byCat[r.Category]; !seen {
			cats = append(cats, r.Category)
		}
		byCat[r.Category] = append(byCat[r.Category], r)
	}
	sort.Strings(cats)

	var gaps []CategoryGap
	for _, cat := range cats {
		defs := byCat[cat]
		var examples []string
		gapped := 0
		for _, r := range defs {
			if bestScore(r, teamProfiles) < coverageMatchThreshold {
				gapped++
				if len(examples) < 5 {
					examples = append(examples, r.Name)
				}
			}
		}
		if gapped > 0 {
			gaps = append(gaps, CategoryGap{Category: cat, Total: len(defs), Gapped: gapped, Examples: examples})
		}
	}
	return gaps
}

// coverageExtra lists team roles with no close reference match — domain-specific,
// informational only, never a "wrong" flag.
func coverageExtra(teamProfiles, refProfiles []roleProfile) []ValidateFinding {
	var out []ValidateFinding
	for _, t := range teamProfiles {
		score := bestScore(t, refProfiles)
		if len(refProfiles) == 0 || score < coverageMatchThreshold {
			out = append(out, ValidateFinding{
				Category: CategoryCoverageExtra, Def: t.File,
				Detail: fmt.Sprintf("%q has no close reference match — domain-specific, informational only", t.Name),
			})
		}
	}
	sortFindings(out)
	return out
}

// FormatValidateResult renders res as `team validate`'s human-readable report —
// grouped sections + the one-line Summary().
func FormatValidateResult(res ValidateResult) string {
	var b strings.Builder
	section := func(title string, findings []ValidateFinding) {
		if len(findings) == 0 {
			return
		}
		fmt.Fprintf(&b, "== %s (%d) ==\n", title, len(findings))
		for _, f := range findings {
			if f.Def != "" {
				fmt.Fprintf(&b, "  %s: %s\n", f.Def, f.Detail)
			} else {
				fmt.Fprintf(&b, "  %s\n", f.Detail)
			}
		}
	}
	section("hygiene", res.Hygiene)
	section("tool-scoping", res.ToolScoping)
	section("quality nudges", res.Quality)

	if len(res.CoverageGaps) > 0 {
		fmt.Fprintf(&b, "== coverage gaps by reference category (%d) ==\n", len(res.CoverageGaps))
		for _, g := range res.CoverageGaps {
			fmt.Fprintf(&b, "  %s: %d/%d reference role(s) with no close team match (e.g. %s)\n",
				g.Category, g.Gapped, g.Total, strings.Join(g.Examples, ", "))
		}
	}
	section("team-only roles (informational)", res.CoverageExtra)

	// Starter drift, ordered so the only actionable status comes first. Customised and
	// local definitions are the GOAL, so they are counted rather than listed: a report that
	// prints a line per edited definition trains people to skim past it, and then the two
	// lines that matter go unread with everything else.
	if len(res.StarterDrift) > 0 {
		n := starter.CountByStatus(res.StarterDrift)
		fmt.Fprintf(&b, "== vs shipped starters ==\n")
		if res.InstalledDir != "" {
			fmt.Fprintf(&b, "  compared with the definitions installed in %s\n", res.InstalledDir)
		}
		for _, f := range res.StarterDrift {
			if f.Status == starter.DriftPristine {
				fmt.Fprintf(&b, "  %s: %s\n", f.Def, f.Detail)
			}
		}
		// Not-installed starters are COLLAPSED to one line with a few examples, the same
		// convention the reference-coverage section uses. Listing all of them produced
		// eleven lines for a team deliberately running a subset, which buries the single
		// actionable finding above it — the exact noise this section exists to avoid.
		var removed []string
		for _, f := range res.StarterDrift {
			if f.Status == starter.DriftRemoved {
				removed = append(removed, strings.TrimSuffix(f.Def, ".md"))
			}
		}
		if len(removed) > 0 {
			shown := removed
			suffix := ""
			if len(shown) > 5 {
				shown, suffix = shown[:5], ", …"
			}
			fmt.Fprintf(&b, "  %d starter(s) not installed, left alone (%s%s)\n",
				len(removed), strings.Join(shown, ", "), suffix)
		}
		fmt.Fprintf(&b, "  (%d definition(s) are yours, %d local to this team — overriding a starter is the intended use, not drift)\n",
			n[starter.DriftCustomised], n[starter.DriftLocal])
	}

	fmt.Fprintf(&b, "summary: %s\n", res.Summary())
	return b.String()
}

func sortFindings(f []ValidateFinding) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Def != f[j].Def {
			return f[i].Def < f[j].Def
		}
		return f[i].Detail < f[j].Detail
	})
}
