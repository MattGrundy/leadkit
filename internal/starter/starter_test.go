package starter

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// The whole point of embedding is that the binary needs no checkout, so the assets must be
// present and non-trivial in a built binary. An empty embed compiles fine and fails later.
func TestEmbeddedAssets_ArePresentAndSubstantial(t *testing.T) {
	if len(ProfileTemplate()) < 500 {
		t.Errorf("profile template is %d bytes — suspiciously small for an embed", len(ProfileTemplate()))
	}
	if len(TeamProtocol()) < 500 {
		t.Errorf("team protocol is %d bytes — suspiciously small for an embed", len(TeamProtocol()))
	}
}

// The embedded profile must actually parse as YAML. Shipping a template that a user copies
// and then cannot load would be worse than shipping nothing.
func TestProfileTemplate_IsValidYAMLWithTheKeysItDocuments(t *testing.T) {
	var m map[string]any
	if err := yaml.Unmarshal(ProfileTemplate(), &m); err != nil {
		t.Fatalf("embedded profile is not valid YAML: %v", err)
	}
	for _, k := range []string{"project_id", "queue_root", "store_root", "enforced_remotes", "push_whitelist", "roster"} {
		if _, ok := m[k]; !ok {
			t.Errorf("template is missing %q, which its own comments describe", k)
		}
	}
	// enforced_remotes must ship EMPTY: that means "gate this project's own origin" and
	// names nothing. A populated default would govern the wrong repo on every adopter.
	if v, ok := m["enforced_remotes"].([]any); !ok || len(v) != 0 {
		t.Errorf("enforced_remotes must be an empty list in the template, got %#v", m["enforced_remotes"])
	}
	// The gate needs code-review and critique as DIFFERENT roles, so the starter roster
	// must contain both or a fresh adopter cannot satisfy their own gate.
	roster, _ := yaml.Marshal(m["roster"])
	for _, role := range []string{"code-review", "critique"} {
		if !strings.Contains(string(roster), role) {
			t.Errorf("starter roster omits %q; the gate needs both layers as separate roles", role)
		}
	}
}

// Both assets are starters meant to be replaced, so they must be identifiable as generated.
func TestProtocol_IsMarkedGenerated(t *testing.T) {
	if !IsGeneratedProtocol(TeamProtocol()) {
		t.Fatal("the embedded protocol must carry the generated marker")
	}
	if IsGeneratedProtocol([]byte("# Our own protocol\n")) {
		t.Error("a human-written file must not be mistaken for generated")
	}
	// Leading blank lines must not defeat detection, since an editor may add them.
	if !IsGeneratedProtocol([]byte("\n\n  " + ProtocolMarker + "\nrest")) {
		t.Error("detection must tolerate leading whitespace")
	}
}

// Neither asset may name a real project, org or person: they are embedded in every binary,
// so a leak here travels further and more silently than one in a repo file.
func TestEmbeddedAssets_NameNobody(t *testing.T) {
	for name, data := range map[string][]byte{"profile": ProfileTemplate(), "protocol": TeamProtocol()} {
		low := strings.ToLower(string(data))
		for _, tok := range append([]string{"planka", "/users/"}, scrubterms.Optional(t).ForTests()...) {
			if strings.Contains(low, tok) {
				t.Errorf("%s asset contains %q", name, tok)
			}
		}
	}
}

// Adoption must be safe to re-run. Someone will run init twice, and silently replacing a
// profile a team has tuned is the worst available behaviour for a convenience command.
func TestWrite_IsCreateOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "leadkit.yaml")

	if err := Write(path, ProfileTemplate()); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != len(ProfileTemplate()) {
		t.Errorf("wrote %d bytes, template is %d", len(got), len(ProfileTemplate()))
	}

	// Second run must refuse, and must refuse with a sentinel the caller can recognise
	// rather than a string it has to match on.
	err = Write(path, []byte("replacement\n"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second write error = %v, want ErrExists", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(got) {
		t.Error("the existing file was modified; create-only means create-only")
	}
}

// An interrupted write must not leave a partial profile that parses as valid-but-wrong, and
// must not leave temp files behind to confuse the next run.
func TestWrite_LeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "leadkit.yaml"), ProfileTemplate()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".leadkit-starter-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("want exactly the written file, got %d entries", len(entries))
	}
}

// The returned slices must be copies: a caller mutating one must not corrupt the embedded
// asset for everything else in the process.
func TestAccessors_ReturnCopies(t *testing.T) {
	a := ProfileTemplate()
	if len(a) == 0 {
		t.Fatal("empty template")
	}
	a[0] = 'X'
	if ProfileTemplate()[0] == 'X' {
		t.Error("mutating the returned slice corrupted the embedded asset")
	}
}

// --- starter agent definitions ---

// agentFrontmatter pulls the YAML frontmatter block, which the harness requires FIRST.
func agentFrontmatter(t *testing.T, data []byte) map[string]string {
	t.Helper()
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("definition must open with YAML frontmatter; the harness cannot parse it otherwise")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		t.Fatal("unterminated frontmatter")
	}
	out := map[string]string{}
	for _, line := range strings.Split(s[4:4+end], "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func TestStarterAgents_HaveParseableFrontmatterAndAreMarked(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatalf("Agents(): %v", err)
	}
	if len(agents) < 10 {
		t.Fatalf("expected a full starter set, got %d definitions", len(agents))
	}
	for name, data := range agents {
		fm := agentFrontmatter(t, data)
		for _, k := range []string{"name", "description", "tools", "model"} {
			if fm[k] == "" {
				t.Errorf("%s: frontmatter missing %q", name, k)
			}
		}
		// The file name must match the declared name, or spawning by name silently gets
		// the wrong definition.
		if want := strings.TrimSuffix(name, ".md"); fm["name"] != want {
			t.Errorf("%s: declares name %q, want %q to match the file", name, fm["name"], want)
		}
		if !IsGeneratedAgent(data) {
			t.Errorf("%s: missing the generated-starter marker", name)
		}
	}
}

// Least privilege, from the sandboxing design: an agent without a tool cannot misuse it.
// Reviewers and read-only roles must not carry Edit or Write. This checks the tool LIST, not
// the capability: every one of these holds Bash, which can write, commit and push, so what
// actually keeps a reviewer off the diff is the prose rule pinned in
// TestStarterAgents_ReviewersAreToldTheyAreReadOnly, not this list.
func TestStarterAgents_ReadOnlyRolesHaveNoWriteTools(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	readOnly := []string{
		"code-review-agent.md", "critique-agent.md",
		"recon-agent.md", "test-runner.md", "orchestrator.md", "architecture-agent.md",
		// finalise ships the work, so an Edit grant would put code into a PR that neither
		// gate layer reviewed — the one change nobody would see.
		"finalise.md",
	}
	for _, name := range readOnly {
		data, ok := agents[name]
		if !ok {
			t.Errorf("expected a %s definition in the starter set", name)
			continue
		}
		tools := agentFrontmatter(t, data)["tools"]
		for _, forbidden := range []string{"Edit", "Write"} {
			if strings.Contains(tools, forbidden) {
				t.Errorf("%s is a read-only role but its tools include %s: %q", name, forbidden, tools)
			}
		}
	}
}

// The gate needs code-review and critique as INDEPENDENT roles, so both must ship or a
// fresh adopter cannot satisfy their own gate. The lead must not be a reviewer.
func TestStarterAgents_GateRolesAreIndependent(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for _, needed := range []string{"code-review-agent.md", "critique-agent.md"} {
		if _, ok := agents[needed]; !ok {
			t.Errorf("the gate requires %s; a starter set without it ships an unsatisfiable gate", needed)
		}
	}
	lead, ok := agents["lead.md"]
	if !ok {
		t.Fatal("no lead definition")
	}
	low := strings.ToLower(string(lead))
	if !strings.Contains(low, "do not write the code") && !strings.Contains(low, "does not implement") {
		t.Error("the lead definition must state that it does not implement; that rule is load-bearing")
	}
}

// The overarching lead is a shipped role in its own right. Nothing else names the file, so a
// deleted file, a renamed directory or a changed embed line would otherwise stop shipping it
// with every other test green. The sentence pinned here is the one the role exists to carry:
// a downstream session that reads a relay as approval is the near-miss it prevents.
func TestStarterAgents_OverarchingLeadShipsAndStatesRelayIsNotAuthorization(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	def, ok := agents["overarching-lead.md"]
	if !ok {
		t.Fatal("the starter set must ship overarching-lead.md")
	}
	if got := agentFrontmatter(t, def)["name"]; got != "overarching-lead" {
		t.Errorf("overarching-lead.md declares name %q", got)
	}
	text := words(def)
	if !strings.Contains(text, "**Relay is not authorization.**") {
		t.Error("overarching-lead must state that relay is not authorization")
	}
	if !strings.Contains(text, "It NEVER authorizes a privileged mutation") {
		t.Error("overarching-lead must state that it never authorizes a privileged mutation")
	}
	// The verbs, the right way round: this seat RECEIVES escalate and USES route. The first
	// draft had them backwards (escalate "to the human", route "across").
	if !strings.Contains(text, "`leadkit brief route <id> --to <project>`") {
		t.Error("overarching-lead must name `leadkit brief route` as the way it places an escalated brief")
	}
	for _, wrong := range []string{"(up, to the human)", "(across, to another lead)"} {
		if strings.Contains(text, wrong) {
			t.Errorf("overarching-lead misdescribes the brief verbs: %q", wrong)
		}
	}
}

// Two roles are deliberately absent, and each absence is documented so it reads as a
// decision rather than an oversight. Both were shipped in a first draft of this set by
// transcribing a role list without checking whether each role was still live, which is
// exactly how a retired pattern gets taught to every future adopter.
func TestStarterAgents_NoPollerAndTheReasonIsRecorded(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	// A polling agent burns tokens per tick where a blocking command costs nothing.
	if _, ok := agents["poller-agent.md"]; ok {
		t.Error("the starter set must not ship a polling agent")
	}
	// The review gate is TWO layers. A third verifier added process without adding an
	// independent question, and its checks belong in critique.
	if _, ok := agents["outcome-verifier.md"]; ok {
		t.Error("the starter set must not ship a third gate layer; the gate is two layers")
	}
	crit, ok := agents["critique-agent.md"]
	if !ok {
		t.Fatal("no critique definition")
	}
	if !strings.Contains(string(crit), "TWO layers") {
		t.Error("critique must state that it is the terminal layer, or a reader will assume a third exists")
	}
	for _, folded := range []string{"stated goal", "checkable", "Hygiene"} {
		if !strings.Contains(string(crit), folded) {
			t.Errorf("critique is missing %q, folded in from the retired third layer", folded)
		}
	}
	var documented bool
	for _, data := range agents {
		if strings.Contains(string(data), "no polling agent") {
			documented = true
		}
	}
	if !documented {
		t.Error("the absence of a poller must be explained somewhere in the set")
	}
}

// Same rule as the other embedded assets, and it matters more here: these ship in every
// binary, so a name in one travels silently.
func TestStarterAgents_NameNobody(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range agents {
		low := strings.ToLower(string(data))
		for _, tok := range append([]string{"planka", "argo", "/users/", "turbo"}, scrubterms.Optional(t).ForTests()...) {
			if strings.Contains(low, tok) {
				t.Errorf("%s contains %q", name, tok)
			}
		}
	}
}

// words collapses runs of whitespace, so a pin on a sentence survives being re-wrapped.
func words(b []byte) string { return strings.Join(strings.Fields(string(b)), " ") }

func toolList(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	for _, tool := range strings.Split(agentFrontmatter(t, data)["tools"], ",") {
		out = append(out, strings.TrimSpace(tool))
	}
	return out
}

// The session task pool is retired as a gate system of record, so no role may be granted
// its tools: a grant is an invitation to the mechanism the design removed.
func TestStarterAgents_NoRoleHoldsTheRetiredTaskTools(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range agents {
		for _, tool := range toolList(t, data) {
			if strings.HasPrefix(tool, "Task") {
				t.Errorf("%s is granted %s; the session task pool is not the system of record", name, tool)
			}
		}
	}
}

// Their output is a verdict recorded on disk. A reviewer that can message can be nudged
// into agreement, and the independence of the two layers is the reason there are two.
func TestStarterAgents_ReviewersCannotBeNudgedByMessage(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code-review-agent.md", "critique-agent.md"} {
		for _, tool := range toolList(t, agents[name]) {
			if tool == "SendMessage" {
				t.Errorf("%s holds SendMessage; a reviewer must report by recorded verdict only", name)
			}
		}
	}
}

var (
	gateRecordCmd = regexp.MustCompile(`gate\s+record`)
	layerFlag     = regexp.MustCompile("--layer(?:=|\\s+)([^\\s`\"'.,;)]+)")
)

// recordedLayers returns the --layer of every `gate record` command in text. A command is read from
// `gate record` to the end of its paragraph or code fence, and read as one line, so it cannot be hidden
// from the pin by a line wrap, a backslash continuation, extra spaces, `--layer=x`, or putting --layer
// after another flag. A `gate record` with no --layer in reach yields "" so the caller sees it.
func recordedLayers(text string) []string {
	var layers []string
	for _, loc := range gateRecordCmd.FindAllStringIndex(text, -1) {
		seg := text[loc[0]:]
		if i := strings.Index(seg, "\n\n"); i >= 0 {
			seg = seg[:i]
		}
		if i := strings.Index(seg, "```"); i >= 0 {
			seg = seg[:i]
		}
		seg = strings.Join(strings.Fields(strings.ReplaceAll(seg, "\\\n", " ")), " ")
		if m := layerFlag.FindStringSubmatch(seg); m != nil {
			layers = append(layers, m[1])
		} else {
			layers = append(layers, "")
		}
	}
	return layers
}

// The pin on reviewer independence is only worth having if a reviewer definition cannot be made to
// instruct the wrong layer by reflowing the command. Each case is a rewrap, respacing or requoting that a
// single-line, single-space match would miss.
func TestRecordedLayers_ReadsACommandHoweverItIsWrapped(t *testing.T) {
	cases := map[string]string{
		"one line":              "leadkit gate record --layer critique --verdict ready",
		"wrapped after record":  "leadkit gate record\n--layer critique --verdict ready",
		"wrapped before layer":  "leadkit gate record --verdict ready\n  --layer critique",
		"backslash continued":   "leadkit gate record \\\n  --layer critique \\\n  --verdict ready",
		"double spaced":         "leadkit gate  record   --layer   critique",
		"equals form":           "leadkit gate record --layer=critique --verdict ready",
		"flag order swapped":    "leadkit gate record --by me --verdict ready --layer critique",
		"inside a code span":    "run `leadkit gate record --layer critique --verdict ready` now",
		"split across the word": "leadkit gate\nrecord --layer critique",
	}
	for name, text := range cases {
		if got := recordedLayers(text); len(got) != 1 || got[0] != "critique" {
			t.Errorf("%s: recordedLayers = %q, want [critique]", name, got)
		}
	}
	if got := recordedLayers("leadkit gate record --layer code-review\n\nrun leadkit gate record\n--layer critique"); len(got) != 2 || got[0] != "code-review" || got[1] != "critique" {
		t.Errorf("two commands must give two layers: %q", got)
	}
	// Prose that mentions the other layer without a command is not a command.
	if got := recordedLayers("leadkit gate record --layer critique --sha x\n\n`--layer code-review` is not yours to write"); len(got) != 1 || got[0] != "critique" {
		t.Errorf("a later paragraph is not part of the command: %q", got)
	}
	// A command with no layer is reported, not dropped.
	if got := recordedLayers("leadkit gate record --verdict ready"); len(got) != 1 || got[0] != "" {
		t.Errorf("a layerless command must be visible: %q", got)
	}
}

// Each reviewer must be told to record ITS OWN layer and no other. The two layers are only
// independent if neither role is ever instructed to write the other's verdict.
func TestStarterAgents_EachReviewerRecordsOnlyItsOwnLayer(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for name, layer := range map[string]string{"code-review-agent.md": "code-review", "critique-agent.md": "critique"} {
		layers := recordedLayers(string(agents[name]))
		if len(layers) == 0 {
			t.Errorf("%s never names the recording command, so the verdict has no mechanism", name)
		}
		for _, got := range layers {
			if got != layer {
				t.Errorf("%s instructs recording --layer %q; its own layer is %s", name, got, layer)
			}
		}
	}
	crit := words(agents["critique-agent.md"])
	for _, phrase := range []string{"Record only your own layer", "is not yours to write"} {
		if !strings.Contains(crit, phrase) {
			t.Errorf("critique must say %q", phrase)
		}
	}
	cr := words(agents["code-review-agent.md"])
	if !strings.Contains(cr, "do not record critique's layer") {
		t.Error("code-review must refuse to record critique's layer in words, not only by omission")
	}
}

// The tool list cannot stop a reviewer holding Bash from editing, so the prose is the control.
func TestStarterAgents_ReviewersAreToldTheyAreReadOnly(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(words(agents["code-review-agent.md"]), "You never edit, commit or push.") {
		t.Error("code-review must state it never edits, commits or pushes")
	}
}

// test-runner is told which command finds and runs the project's tests, because an agent
// handed a judgment without a mechanism improvises one.
func TestStarterAgents_TestRunnerNamesTheTestCommand(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents["test-runner.md"]), "leadkit test run --repo") {
		t.Error("test-runner must name `leadkit test run`")
	}
}

// Shipped text must not name a mechanism the binary lacks: it is embedded in every binary,
// so a false claim reaches every adopter, and an agent told a command validates something
// will rely on it. Each phrase here was shipped once and was false. This pins those
// specific claims; it does not check assets against the command surface in general.
func TestEmbeddedAssets_DoNotClaimMechanismsThatDoNotExist(t *testing.T) {
	assets := map[string][]byte{"profile": ProfileTemplate(), "protocol": TeamProtocol()}
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range agents {
		assets[name] = data
	}
	for _, c := range []struct{ phrase, why string }{
		{"validates the transition", "`queue move` checks source, root and destination only, not whether the transition is valid"},
		{"--write-profile", "`init` has no profile mode; `config init` writes a profile"},
		{"`team status` tells", "`team status` does not read the protocol marker"},
		{"approves work-prep", "an overarching lead relays; \"approves\" reads as a green light, and relay is not authorization"},
	} {
		for name, data := range assets {
			if strings.Contains(words(data), c.phrase) {
				t.Errorf("%s says %q, which is false: %s", name, c.phrase, c.why)
			}
		}
	}
}

// Write chmods the temp file because CreateTemp creates 0600, and a profile only its writer
// can read is not what an adopter expects from a file they are meant to share and commit.
func TestWrite_ResultIsWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadkit.yaml")
	if err := Write(path, ProfileTemplate()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

func TestTeamProtocol_ReturnsACopy(t *testing.T) {
	a := TeamProtocol()
	a[0] = 'X'
	if TeamProtocol()[0] == 'X' {
		t.Error("mutating the returned slice corrupted the embedded protocol")
	}
}

// Two separate guards over the `queue move` guidance. This one checks EXAMPLE SYNTAX only:
// well-formed example lines say nothing about whether the prose around them is true.
//
// The claims are covered by the second test below, by forbidding specific false statements.
// A banned-phrase list only catches the phrasings it lists, but it catches a regression of
// those exactly.
func TestStarterDocs_QueueMoveInvocationsParse(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string][]byte{"team-protocol.md": TeamProtocol()}
	for name, data := range agents {
		sources[name] = data
	}

	checked := 0
	for name, data := range sources {
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(trimmed, "leadkit queue move") {
				continue
			}
			// Only shell-example lines, not prose that mentions the command by name.
			if !strings.HasPrefix(trimmed, "leadkit queue move") {
				continue
			}
			// Drop a trailing `# comment`.
			if idx := strings.Index(trimmed, "#"); idx >= 0 {
				trimmed = strings.TrimSpace(trimmed[:idx])
			}
			fields := strings.Fields(trimmed)[3:] // past "leadkit queue move"
			var positionals, flagsAfterPositional int
			// --queue takes a VALUE, so its argument must not be counted as a positional.
			// Single-dash is a flag too: Go's flag package accepts -dry-run and --dry-run alike.
			skipNext := false
			for _, f := range fields {
				if skipNext {
					skipNext = false
					continue
				}
				if strings.HasPrefix(f, "-") {
					if positionals > 0 {
						flagsAfterPositional++
					}
					if strings.TrimLeft(f, "-") == "queue" {
						skipNext = true
					}
					continue
				}
				positionals++
			}
			checked++
			if flagsAfterPositional > 0 {
				t.Errorf("%s:%d: %q puts a flag AFTER a positional — Go's flag parsing stops at the first positional, so it is swallowed as an argument",
					name, i+1, trimmed)
			}
			if positionals != 2 {
				t.Errorf("%s:%d: %q has %d positional(s); `queue move` requires exactly <from> and <to>, and omitting <to> is how a trailing flag becomes the destination",
					name, i+1, trimmed, positionals)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no `leadkit queue move` examples found in the starter docs — this guard went inert, or the docs stopped showing the command they tell an agent to run")
	}
}

// Forbids false assertions about what `queue move` checks, in every shipped starter asset,
// since one file's fix can miss an identical claim in another.
func TestStarterDocs_NoFalseClaimsAboutQueueMove(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string][]byte{"team-protocol.md": TeamProtocol()}
	for name, data := range agents {
		sources[name] = data
	}

	// Each entry is a statement that must not appear. `queue move` checks only that the
	// source exists, that both paths resolve inside the queue root, and that the destination
	// is free. It validates no lane, status or ordering.
	banned := []string{
		"validates the transition",
		"queue-relative",
	}
	for name, data := range sources {
		low := strings.ToLower(string(data))
		for _, phrase := range banned {
			if strings.Contains(low, phrase) {
				t.Errorf("%s claims %q about queue move; it checks only source existence, "+
					"queue-root containment and a free destination", name, phrase)
			}
		}
	}
}

// The criteria are the definitions' contract with each other: code-review's definition defers
// the duplication question to critique, so deleting critique's "Reuse and scope" would leave
// no shipped agent asking it. This pins each criterion by a short stable phrase, so removing
// one fails the build instead of quietly narrowing the gate. It does not pin wording, only
// that the question is still asked somewhere in that role.
func TestStarterAgents_GateCriteriaStillAsked(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for name, phrases := range gateCriteria() {
		data, ok := agents[name]
		if !ok {
			t.Errorf("%s missing from the starter set", name)
			continue
		}
		for _, phrase := range phrases {
			if !strings.Contains(string(data), phrase) {
				t.Errorf("%s no longer asks about %q — a gate criterion was removed; if that is deliberate, move it to the other layer and update this list",
					name, phrase)
			}
		}
	}
}

// gateCriteria is the pin table, shared so the guard and its own mutation proof cannot drift.
//
// Pins are the BOLD, PERIOD-TERMINATED form. A bare "Proportion" is a substring of
// "**Proportion of the prose.**", so it would stay green after the criterion it names is
// deleted.
func gateCriteria() map[string][]string {
	return map[string][]string{
		"critique-agent.md": {
			"**Goal satisfaction.**",   // does the diff do what was asked
			"**Silent regressions.**",  // a narrowed check or a widened permission
			"**Proportion.**",          // was a simpler option considered
			"**Reuse and scope.**",     // duplication — code-review defers it here
			"narrates history",         // comment-block history is critique's prose-proportion fault
			"leadkit task note",        // the only way a review note reaches a ticket that has been routed
			"Never probe with a write", // the gate enforces the chain and names the state; a probe verdict is unearned
		},
		"code-review-agent.md": {
			"## Test delta",                               // a behavioural change with no test movement is unverified
			"## Comments",                                 // history never, restatement never, why as short as its constraint
			"godoc, the surface adopters read",            // restating an exported doc comment is a finding whose fix is a rewrite, never deletion
			"EVERY implementation behind the symbol",      // a rewritten doc comment can assert something false of one path
			"what breaks if the next person changes this", // the operative test for a why-comment
			"leadkit task note",                           // the only way a review note reaches a ticket that has been routed
			"Never probe with a write",                    // no layer above this one to check first; a probe verdict is unearned
		},
		"finalise.md": {
			"leadkit gate show", // the read-only way to see a record, instead of cat-ing the JSON
		},
	}
}

// The guard above is only as good as its pins. This proves each pin can fire, without a
// separate mutation run: a pin that still matches after its own single occurrence is removed
// is satisfied by some OTHER line (a bare word substring-matches the bold form), so it cannot
// detect that criterion going missing.
func TestStarterAgents_GateCriteriaPinsAreNotVacuous(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	for name, phrases := range gateCriteria() {
		data, ok := agents[name]
		if !ok {
			t.Errorf("%s missing from the starter set", name)
			continue
		}
		for _, phrase := range phrases {
			mutated := strings.Replace(string(data), phrase, "", 1)
			if mutated == string(data) {
				t.Errorf("%s: pin %q is absent, so it can never fire", name, phrase)
				continue
			}
			if strings.Contains(mutated, phrase) {
				t.Errorf("%s: pin %q still matches after removing its first occurrence — it is "+
					"satisfied by another line and cannot detect this criterion being deleted",
					name, phrase)
			}
		}
	}
}

// protocolRules is the pin table for the operating rules the shared protocol carries. Each
// rule is pinned twice, by its heading and by a phrase from its REASONING, because a rule
// shipped without its reason gets re-derived in the obvious, wrong direction. Phrases are
// matched against whitespace-collapsed text so a re-wrap does not break them, and each must
// occur exactly once (see TestTeamProtocol_RulePinsAreNotVacuous).
func protocolRules() []string {
	return []string{
		"## Waiting is free; a timeout is not an ending",
		"leadkit queue wait <lane> 36000 10",
		"Waiting looks expensive",
		"the wait IS that agent's idle loop",
		"## Reporting is not a reason to end",
		"the ticket file is.**",
		"ending costs the agent its place in the pool",
		"## Behind main is the normal state",
		"O(n squared) rebases and re-gates",
		"integration is the merge commit's job",
		"A workflow that triggers on `pull_request` and passes no `ref:`",
		"is not re-triggered when the base branch moves",
		"the second attempt checked out the identical merge commit as the first",
		"decide by overlap, not by age",
		"a base further back than what was read vouches for commits nobody read",
		"a project whose CI has no such trigger has no net",
		"carries commits belonging to another change",
	}
}

// The rules are protocol, not per-role. Deleting one, or trimming it to the bare rule, must
// fail the build: a rule without its reason gets re-derived in the wrong direction.
func TestTeamProtocol_OperatingRulesAndTheirReasonsStay(t *testing.T) {
	text := words(TeamProtocol())
	for _, phrase := range protocolRules() {
		if !strings.Contains(text, words([]byte(phrase))) {
			t.Errorf("team-protocol.md no longer contains %q — an operating rule or the reason that stops it being re-derived wrongly was removed", phrase)
		}
	}
}

// Proves each pin above can fire: a phrase that still matches after its first occurrence is
// removed is satisfied by another line, so deleting the rule would go unnoticed.
func TestTeamProtocol_RulePinsAreNotVacuous(t *testing.T) {
	text := words(TeamProtocol())
	for _, phrase := range protocolRules() {
		p := words([]byte(phrase))
		mutated := strings.Replace(text, p, "", 1)
		if mutated == text {
			t.Errorf("pin %q is absent, so it can never fire", phrase)
			continue
		}
		if strings.Contains(mutated, p) {
			t.Errorf("pin %q still matches after removing its first occurrence — it cannot detect the rule being deleted", phrase)
		}
	}
}

// The reviewer definitions are what makes mutation evidence a demand on the role rather than a flag
// nobody is told about. Pin the load-bearing sentences so an edit cannot quietly drop one.
func TestReviewerDefinitions_DemandMutationEvidenceAndNameItsTraps(t *testing.T) {
	agents, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	common := []string{"--mutants", "--no-mutant", "-count=1", "COMPILE", "CANNOT FAIL", "REASON"}
	want := map[string][]string{
		"code-review-agent.md": append([]string{"discriminates", "neutral cwd", "non-colliding ids", "snapshot", "allowlist", "another layer"}, common[:5]...),
		"critique-agent.md":    {"--mutants", "--no-mutant", "-count=1", "compile", "Could each assertion fail", "not recorded"},
	}
	for file, needles := range want {
		body, ok := agents[file]
		if !ok {
			t.Fatalf("no embedded %s", file)
		}
		for _, n := range needles {
			if !strings.Contains(string(body), n) {
				t.Errorf("%s no longer says %q", file, n)
			}
		}
	}
	if !strings.Contains(string(agents["code-review-agent.md"]), common[5]) {
		t.Errorf("code-review-agent.md lost the reason-mutation technique")
	}
}
