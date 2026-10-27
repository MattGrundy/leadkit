package team

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/starter"
)

// validate's starter section has one job: say the one thing worth acting on and stay quiet
// about the rest. A report that prints a line per overridden definition gets skimmed, and
// then the lines that matter are skimmed with it.
func TestFormatValidateResult_StarterSection_IsQuietAboutTheExpectedCase(t *testing.T) {
	dir := t.TempDir()
	embedded, err := starter.Agents()
	if err != nil {
		t.Fatal(err)
	}
	// Two pristine (actionable), one overridden, three local, the rest not installed. The
	// counts are all different so a swapped pair of them cannot read the same.
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	local := func(name string) string {
		return "---\nname: " + name + "\ndescription: local\ntools: Read\nmodel: sonnet\n---\nlocal only\n"
	}
	write("lead.md", string(embedded["lead.md"]))
	write("developer.md", string(embedded["developer.md"]))
	write("critique-agent.md", "---\nname: critique-agent\ndescription: ours\ntools: Read\nmodel: sonnet\n---\nour own rules\n")
	write("etl-agent.md", local("etl-agent"))
	write("payroll-agent.md", local("payroll-agent"))
	write("zz-agent.md", local("zz-agent"))
	installed := map[string]bool{"lead.md": true, "developer.md": true, "critique-agent.md": true}

	res, err := Validate(dir, dir, "")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := FormatValidateResult(res)
	t.Log("\n" + out)

	if !strings.Contains(out, "vs shipped starters") {
		t.Fatal("the starter section must appear")
	}
	// Everything below is judged inside the starter section only. lead.md also appears
	// elsewhere in the report (team-only roles), so a whole-report search proves nothing
	// about the section.
	section := out[strings.Index(out, "== vs shipped starters =="):]
	section = section[:strings.Index(section, "summary:")]
	lines := strings.Split(strings.TrimSpace(section), "\n")

	// The pristine ones are named, each on its own finding line, because they are the only
	// actionable finding.
	for _, def := range []string{"lead.md", "developer.md"} {
		var found bool
		for _, line := range lines {
			if strings.HasPrefix(line, "  "+def+": ") && strings.Contains(line, "unchanged from the shipped starter") {
				found = true
			}
		}
		if !found {
			t.Errorf("a pristine starter (%s) must be named in the section: it still says 'replace me'", def)
		}
	}
	// The overridden and local ones must NOT be listed as findings — that is the intended
	// use. Finding lines start with the definition's file name.
	for _, line := range lines {
		for _, quiet := range []string{"critique-agent", "etl-agent", "payroll-agent", "zz-agent"} {
			if strings.Contains(line, quiet) {
				t.Errorf("%s must not be reported in the starter section: %q", quiet, line)
			}
		}
	}
	// And the report must say so in words, with the counts, so nobody reads them as debt.
	if !strings.Contains(section, "(1 definition(s) are yours, 3 local to this team \u2014 overriding a starter is the intended use, not drift)") {
		t.Errorf("the section must state the yours/local counts and that overriding is intended:\n%s", section)
	}
	if !strings.Contains(out, "vs shipped starters: 1 yours, 2 pristine, "+strconv.Itoa(len(embedded)-len(installed))+" not installed") {
		t.Errorf("the summary counts are wrong or reordered:\n%s", out)
	}

	// Not-installed starters must be COLLAPSED, not listed one per line. Running it for
	// real produced eleven lines that buried the single actionable finding, which is the
	// noise this section exists to avoid. Five examples are shown and the rest elided.
	var notInstalled []string
	for name := range embedded {
		if !installed[name] {
			notInstalled = append(notInstalled, strings.TrimSuffix(name, ".md"))
		}
	}
	sort.Strings(notInstalled)
	if len(notInstalled) <= 5 {
		t.Fatalf("fixture must leave more than five starters uninstalled to exercise the elision, got %d", len(notInstalled))
	}
	wantLine := "  " + strconv.Itoa(len(notInstalled)) + " starter(s) not installed, left alone (" +
		strings.Join(notInstalled[:5], ", ") + ", \u2026)"
	var notInstalledLines []string
	for _, line := range lines {
		if strings.Contains(line, "not installed") {
			notInstalledLines = append(notInstalledLines, line)
		}
	}
	if len(notInstalledLines) != 1 || notInstalledLines[0] != wantLine {
		t.Errorf("not-installed starters must collapse to one elided line\n got: %q\nwant: %q", notInstalledLines, wantLine)
	}
	// The section must stay short enough to read at a glance: header, two pristine, the
	// collapsed line, the counts line.
	if len(lines) > 6 {
		t.Errorf("the starter section is %d lines; it should be glanceable", len(lines))
	}
}

// Drift must still be reported when there are no definitions installed at all, because that
// is exactly the state where knowing what is available matters most.
func TestValidate_StarterDrift_SurvivesAMissingAgentsDir(t *testing.T) {
	res, err := Validate(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent"), "")
	// The read error is still returned (callers rely on it), but the drift section must
	// have been populated BEFORE that failure.
	if err == nil {
		t.Log("no error returned for a missing dir; acceptable")
	}
	if len(res.StarterDrift) == 0 {
		t.Fatal("starter drift must be reported even when the agents dir is missing")
	}
	for _, f := range res.StarterDrift {
		if f.Status != starter.DriftRemoved {
			t.Errorf("%s: want every starter reported not-installed, got %q", f.Def, f.Status)
		}
	}
}

// The elision boundary: five not-installed starters are listed whole, a sixth turns the line into five
// examples and an ellipsis. The report above has ten uninstalled, so it cannot tell `> 5` from `> 4` or
// `> 6`; these counts sit on either side of the line.
func TestFormatValidateResult_NotInstalledElisionBoundary(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	line := func(n int) string {
		var drift []starter.DriftFinding
		for _, name := range names[:n] {
			drift = append(drift, starter.DriftFinding{Def: name + ".md", Status: starter.DriftRemoved})
		}
		out := FormatValidateResult(ValidateResult{StarterDrift: drift})
		var found []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "not installed, left alone") {
				found = append(found, l)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%d uninstalled: want one collapsed line, got %q\n%s", n, found, out)
		}
		return found[0]
	}
	cases := []struct {
		n    int
		want string
	}{
		{4, "  4 starter(s) not installed, left alone (alpha, bravo, charlie, delta)"},
		{5, "  5 starter(s) not installed, left alone (alpha, bravo, charlie, delta, echo)"},
		{6, "  6 starter(s) not installed, left alone (alpha, bravo, charlie, delta, echo, …)"},
		{7, "  7 starter(s) not installed, left alone (alpha, bravo, charlie, delta, echo, …)"},
	}
	for _, c := range cases {
		if got := line(c.n); got != c.want {
			t.Errorf("%d uninstalled:\n got %q\nwant %q", c.n, got, c.want)
		}
	}
}

// "Is this starter installed" is a question about the directory the harness loads from, not about the
// team's captured snapshot. The two are independent: a snapshot full of starters says nothing about what
// is installed, and an installed set with no snapshot is fully installed.
func TestValidate_StarterDriftReadsTheInstalledDirNotTheSnapshot(t *testing.T) {
	embedded, err := starter.Agents()
	if err != nil {
		t.Fatal(err)
	}
	populate := func(dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range embedded {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	root := t.TempDir()
	snapshot, installed := filepath.Join(root, "snapshot"), filepath.Join(root, "installed")

	// Every starter is in the snapshot and nothing is installed: all of them are not installed.
	populate(snapshot)
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Validate(snapshot, installed, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := starter.CountByStatus(res.StarterDrift)[starter.DriftRemoved]; n != len(embedded) {
		t.Errorf("nothing is installed, so every starter is not installed: got %d of %d", n, len(embedded))
	}
	if res.InstalledDir != installed {
		t.Errorf("the result must say which directory it measured: %q", res.InstalledDir)
	}
	if out := FormatValidateResult(res); !strings.Contains(out, "installed in "+installed) {
		t.Errorf("the report must name the directory it compared with:\n%s", out)
	}

	// The reverse: everything is installed and the snapshot is empty.
	populate(installed)
	empty := filepath.Join(root, "empty-snapshot")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = Validate(empty, installed, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := starter.CountByStatus(res.StarterDrift)[starter.DriftRemoved]; n != 0 {
		t.Errorf("every starter is installed, so none is not installed: got %d", n)
	}
	if strings.Contains(FormatValidateResult(res), "not installed, left alone") {
		t.Error("a fully installed set must not report anything as not installed")
	}
}
