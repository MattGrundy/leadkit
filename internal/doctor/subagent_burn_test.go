package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var burnNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func burnLine(ts, model, id string, out int) string {
	b, _ := json.Marshal(map[string]any{
		"timestamp": ts,
		"message": map[string]any{
			"id": id, "model": model,
			"usage": map[string]int{"output_tokens": out},
		},
	})
	return string(b)
}

func writeTranscript(t *testing.T, root, proj, name string, lines ...string) {
	t.Helper()
	dir := filepath.Join(root, proj)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture: in-window opus 1M out ($75) + sonnet 1M out ($15) => opus 83%, ~$75; plus a
// HUGE out-of-window opus row that must NOT be counted.
func burnFixture(t *testing.T) string {
	root := t.TempDir()
	writeTranscript(t, root, "-Users-bob-Projects-a", "s.jsonl",
		burnLine("2026-09-29T10:00:00Z", "claude-opus-4-8", "o1", 1_000_000),
		burnLine("2026-09-29T11:00:00Z", "claude-sonnet-5-5", "s1", 1_000_000),
		burnLine("2026-09-01T10:00:00Z", "claude-opus-4-8", "old", 900_000_000))
	return root
}

func settingsWith(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{p}
}

func TestSubagentModel_UnsetWarnsWithOpusShareAndDollars(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	c := subagentModelCheck(settingsWith(t, `{"env":{}}`), burnFixture(t), burnNow)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("severity must stay warn/not-OK: %+v", c)
	}
	for _, want := range []string{"not set", "recent spend (last 3d): opus 83% (~$75), sonnet 17%", "gap this setting closes", "leadkit burn-report --tools"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail missing %q: %s", want, c.Detail)
		}
	}
}

func TestSubagentModel_BoundedWindowExcludesOldRows(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	c := subagentModelCheck(settingsWith(t, `{"env":{}}`), burnFixture(t), burnNow)
	// If the 900M-token 2026-09-01 row leaked in, opus would be ~$67k and ~100%.
	if strings.Contains(c.Detail, "opus 100%") || !strings.Contains(c.Detail, "~$75)") {
		t.Errorf("out-of-window row leaked: %s", c.Detail)
	}
}

func TestSubagentModel_SetSonnetIsInfoOKWithMix(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	c := subagentModelCheck(settingsWith(t, `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"sonnet"}}`), burnFixture(t), burnNow)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("set-to-sonnet must stay OK/info: %+v", c)
	}
	if !strings.Contains(c.Detail, "default subagent model = sonnet") || !strings.Contains(c.Detail, "recent spend (last 3d): opus 83%") {
		t.Errorf("want setting + mix confirmation: %s", c.Detail)
	}
	if strings.Contains(c.Detail, "gap this setting closes") {
		t.Errorf("confirmation must not use the gap wording: %s", c.Detail)
	}
}

func TestSubagentModel_FailSoftNoComparison(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	settings := settingsWith(t, `{"env":{}}`)
	base, _ := subagentModelSetting(settings)
	empty := t.TempDir()
	notADir := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	onlyOld := t.TempDir()
	writeTranscript(t, onlyOld, "-Users-bob-Projects-a", "s.jsonl", burnLine("2026-08-01T10:00:00Z", "claude-opus-4-8", "x", 1000))
	garbage := t.TempDir()
	writeTranscript(t, garbage, "p", "s.jsonl", "{not json", "")
	for name, dir := range map[string]string{
		"unset-param": "", "absent": filepath.Join(t.TempDir(), "nope"), "empty": empty,
		"file-not-dir": notADir, "only-out-of-window": onlyOld, "garbage": garbage,
	} {
		c := subagentModelCheck(settings, dir, burnNow)
		if c.Detail != base.Detail || c.OK != base.OK || c.Severity != base.Severity {
			t.Errorf("%s: want the plain setting verdict, got %+v (base %+v)", name, c, base)
		}
		if strings.Contains(c.Detail, "recent spend") {
			t.Errorf("%s: comparison line must be absent: %s", name, c.Detail)
		}
	}
}

func TestSubagentModel_FileCapHitOmitsPartialLine(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	root := t.TempDir()
	for i := 0; i < burnMaxFiles+5; i++ {
		writeTranscript(t, root, "-Users-bob-Projects-a", fmt.Sprintf("s%d.jsonl", i),
			burnLine("2026-09-29T10:00:00Z", "claude-opus-4-8", fmt.Sprintf("m%d", i), 10))
	}
	c := subagentModelCheck(settingsWith(t, `{"env":{}}`), root, burnNow)
	if strings.Contains(c.Detail, "recent spend") {
		t.Errorf("partial (capped) scan must not print a misleading total: %s", c.Detail)
	}
	if c.Severity != SeverityWarn {
		t.Errorf("severity changed: %+v", c)
	}
}

func TestSubagentModel_SpendNeverChangesSeverity(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	root := t.TempDir()
	// Absurd spend: 100% opus, billions of tokens.
	writeTranscript(t, root, "-Users-bob-Projects-a", "s.jsonl", burnLine("2026-09-29T10:00:00Z", "claude-opus-4-8", "o", 2_000_000_000))
	set := subagentModelCheck(settingsWith(t, `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"sonnet"}}`), root, burnNow)
	if !set.OK || set.Severity != SeverityInfo {
		t.Errorf("heavy opus spend must not degrade a SET check: %+v", set)
	}
	unset := subagentModelCheck(settingsWith(t, `{"env":{}}`), root, burnNow)
	if unset.Severity != SeverityWarn {
		t.Errorf("unset must stay warn, never critical: %+v", unset)
	}
}

func TestRun_SubagentModelCheckCarriesRecentSpend(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "claude-sonnet-5")
	prof := cleanProfile(t)
	root := t.TempDir()
	day := time.Now().UTC().Format(time.DateOnly)
	writeTranscript(t, root, "-Users-bob-Projects-a", "s.jsonl", burnLine(day+"T01:00:00Z", "claude-opus-4-8", "o", 1_000_000))
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", root)
	c, ok := checksByName(report.Checks)["subagent-model"]
	if !ok || !strings.Contains(c.Detail, "recent spend (last 3d)") {
		t.Fatalf("Run must thread transcriptsDir into the check: %+v", c)
	}
}
