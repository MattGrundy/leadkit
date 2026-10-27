package burnreport

import (
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func seq(rs ...usageRecord) iter.Seq[usageRecord] {
	return func(yield func(usageRecord) bool) {
		for _, r := range rs {
			if !yield(r) {
				return
			}
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func day(s string) time.Time { t, _ := time.Parse(time.DateOnly, s); return t }

func rowByKey(rows []Row, k string) (Row, bool) {
	for _, r := range rows {
		if r.Key == k {
			return r, true
		}
	}
	return Row{}, false
}

// jl builds one transcript line.
func jl(ts, model, id string, in, cw, cr, out int, tools ...string) string {
	content := []map[string]any{{"type": "text", "text": "hi"}}
	for i, t := range tools {
		content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%s_%d", id, i), "name": t})
	}
	b, _ := json.Marshal(map[string]any{
		"timestamp": ts,
		"message": map[string]any{
			"id": id, "model": model, "content": content,
			"usage": map[string]int{"input_tokens": in, "cache_creation_input_tokens": cw, "cache_read_input_tokens": cr, "output_tokens": out},
		},
	})
	return string(b)
}

func writeT(t *testing.T, root, dir, name string, lines ...string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collect(root string, tools bool, since, until time.Time) Report {
	return Aggregate(WalkTranscripts(root, tools), DefaultPrices, since, until, tools)
}

func TestBurnReport_AggregationMatchesHandComputed(t *testing.T) {
	root := t.TempDir()
	// project alpha: opus day1 + sonnet day2; project beta: opus day2.
	writeT(t, root, "-Users-bob-Projects-alpha", "s1.jsonl",
		jl("2026-09-23T10:00:00Z", "claude-opus-4-8", "m1", 1000, 2000, 10000, 500),  // opus
		jl("2026-09-24T10:00:00Z", "claude-sonnet-5-5", "m2", 1000, 0, 100000, 2000)) // sonnet
	writeT(t, root, "-Users-bob-Projects-beta", "s2.jsonl",
		jl("2026-09-24T11:00:00Z", "claude-opus-4-8", "m3", 0, 0, 0, 1000))
	rep := collect(root, false, day("2026-09-01"), time.Time{})

	// opus m1: (1000*15 + 2000*18.75 + 10000*1.5 + 500*75)/1e6 = (15000+37500+15000+37500)/1e6 = 0.105
	// sonnet m2: (1000*3 + 0 + 100000*0.3 + 2000*15)/1e6 = (3000+30000+30000)/1e6 = 0.063
	// opus m3: 1000*75/1e6 = 0.075
	if !near(rep.TotalCost, 0.243) {
		t.Fatalf("total = %v want 0.243", rep.TotalCost)
	}
	if rep.Tokens != 13500+103000+1000 {
		t.Fatalf("tokens = %d", rep.Tokens)
	}
	opus, _ := rowByKey(rep.ByModel, "opus")
	son, _ := rowByKey(rep.ByModel, "sonnet")
	if !near(opus.Cost, 0.18) || !near(son.Cost, 0.063) {
		t.Fatalf("model costs opus=%v sonnet=%v", opus.Cost, son.Cost)
	}
	if rep.ByModel[0].Key != "opus" {
		t.Fatalf("by-model not sorted $ desc: %+v", rep.ByModel)
	}
	if !near(opus.Pct, 0.18/0.243*100) {
		t.Fatalf("opus pct %v", opus.Pct)
	}
	alpha, _ := rowByKey(rep.ByProject, "alpha")
	beta, _ := rowByKey(rep.ByProject, "beta")
	if !near(alpha.Cost, 0.168) || !near(beta.Cost, 0.075) {
		t.Fatalf("project costs alpha=%v beta=%v", alpha.Cost, beta.Cost)
	}
	if !near(alpha.OpusPct, 0.105/0.168*100) || !near(beta.OpusPct, 100) {
		t.Fatalf("opus pct alpha=%v beta=%v", alpha.OpusPct, beta.OpusPct)
	}
	d1, _ := rowByKey(rep.ByDay, "2026-09-23")
	d2, _ := rowByKey(rep.ByDay, "2026-09-24")
	if !near(d1.Cost, 0.105) || !near(d2.Cost, 0.138) {
		t.Fatalf("day costs %v %v", d1.Cost, d2.Cost)
	}
	if rep.ByDay[0].Key != "2026-09-23" {
		t.Fatalf("by-day should be chronological: %+v", rep.ByDay)
	}
}

func TestBurnReport_DuplicateMessageCountedOnce(t *testing.T) {
	root := t.TempDir()
	writeT(t, root, "-Users-bob-Projects-a", "s.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "same", 0, 0, 0, 1000),
		jl("2026-09-24T10:00:01Z", "claude-opus-4-8", "same", 0, 0, 0, 1000))
	rep := collect(root, false, day("2026-09-01"), time.Time{})
	if !near(rep.TotalCost, 0.075) {
		t.Fatalf("duplicate message.id double-counted: %v", rep.TotalCost)
	}
}

func TestBurnReport_CostProxyPerFamilyAndOverride(t *testing.T) {
	r := func(model string) usageRecord {
		return usageRecord{Project: "p", Day: "2026-09-24", Model: model, Input: 1_000_000, CacheW: 1_000_000, CacheR: 1_000_000, Output: 1_000_000}
	}
	cases := map[string]float64{
		"claude-opus-4-8":   15 + 18.75 + 1.5 + 75,
		"claude-sonnet-5-5": 3 + 3.75 + 0.3 + 15,
		"claude-haiku-4-5":  0.8 + 1 + 0.08 + 4,
		"<synthetic>":       0,
		"gpt-whatever":      0,
	}
	for model, want := range cases {
		rep := Aggregate(seq(r(model)), DefaultPrices, day("2026-09-01"), time.Time{}, false)
		if !near(rep.TotalCost, want) {
			t.Errorf("%s: cost %v want %v", model, rep.TotalCost, want)
		}
	}
	over := PriceTable{"opus": {1, 2, 3, 4}}
	rep := Aggregate(seq(r("claude-opus-4-8"), r("claude-sonnet-5-5")), over, day("2026-09-01"), time.Time{}, false)
	if !near(rep.TotalCost, 10) { // sonnet absent from override table → 0
		t.Fatalf("override total %v want 10", rep.TotalCost)
	}
}

func TestBurnReport_WindowBoundaries(t *testing.T) {
	mk := func(d string) usageRecord {
		return usageRecord{Project: "p", Day: d, Model: "claude-opus-4-8", Output: 1_000_000}
	}
	rep := Aggregate(seq(mk("2026-09-22"), mk("2026-09-23"), mk("2026-09-25"), mk("2026-09-26")),
		DefaultPrices, day("2026-09-23"), day("2026-09-25"), false)
	if len(rep.ByDay) != 2 || rep.ByDay[0].Key != "2026-09-23" || rep.ByDay[1].Key != "2026-09-25" {
		t.Fatalf("window rows: %+v", rep.ByDay)
	}
	if !near(rep.TotalCost, 150) {
		t.Fatalf("total %v", rep.TotalCost)
	}
}

func TestBurnReport_ToolMix(t *testing.T) {
	root := t.TempDir()
	writeT(t, root, "-Users-bob-Projects-alpha", "s.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "a1", 1, 0, 0, 1, "Edit", "Bash", "Write", "Task"),
		jl("2026-09-24T10:01:00Z", "claude-opus-4-8", "a2", 1, 0, 0, 1, "SendMessage", "Read"),
		// sonnet tool calls must NOT count
		jl("2026-09-24T10:02:00Z", "claude-sonnet-5-5", "a3", 1, 0, 0, 1, "Edit", "Edit", "Edit"))
	writeT(t, root, "-Users-bob-Projects-onlysonnet", "s.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-sonnet-5-5", "b1", 1, 0, 0, 1, "Bash"),
		jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "b2", 1, 0, 0, 1)) // opus, zero tool calls
	rep := collect(root, true, day("2026-09-01"), time.Time{})
	if len(rep.ToolMix) != 1 {
		t.Fatalf("want only alpha in tool mix (no divide-by-zero row), got %+v", rep.ToolMix)
	}
	m := rep.ToolMix[0]
	// 6 opus calls: IC = Edit,Bash,Write =3 ; ORCH = Task,SendMessage =2 ; Read = neither.
	if m.Project != "alpha" || m.Calls != 6 || !near(m.ICPct, 50) || !near(m.OrchPct, 100.0/3) {
		t.Fatalf("mix = %+v", m)
	}
	if rep2 := collect(root, false, day("2026-09-01"), time.Time{}); len(rep2.ToolMix) != 0 {
		t.Fatalf("tool mix present without --tools: %+v", rep2.ToolMix)
	}
}

func TestBurnReport_FailSoft(t *testing.T) {
	root := t.TempDir()
	huge := `{"timestamp":"2026-09-24T10:00:00Z","message":{"model":"claude-opus-4-8","usage":{"output_tokens":999999},"content":"` +
		strings.Repeat("x", maxTranscriptLine+1024) + `"}}`
	writeT(t, root, "-Users-bob-Projects-a", "s.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "g1", 0, 0, 0, 1000), // good
		`{not json at all`,
		`{"timestamp":"2026-09-24T10:00:00Z","message":{"model":"claude-opus-4-8"}}`,                        // no usage
		`{"timestamp":"2026-09-24T10:00:00Z"}`,                                                              // no message
		`{"message":{"model":"claude-opus-4-8","usage":{"output_tokens":5}}}`,                               // no timestamp
		`{"timestamp":"garbage-ts-here","message":{"model":"claude-opus-4-8","usage":{"output_tokens":5}}}`, // bad ts
		`{"timestamp":"2026-09-24T10:00:00Z","message":{"usage":{"output_tokens":5}}}`,                      // no model
		`{"timestamp":"2026-09-24T10:00:00Z","message":{"model":"claude-opus-4-8","content":7,"usage":{"output_tokens":5}}}`,
		huge,
		jl("2026-09-24T10:00:00Z", "mystery-model-9", "g2", 0, 0, 0, 1000), // unknown model: tokens, $0
		jl("2026-09-24T10:00:00Z", "claude-sonnet-5-5", "g3", 0, 0, 0, 1000))
	// also: a final line with no trailing newline, and an empty/binary file
	if err := os.WriteFile(filepath.Join(root, "-Users-bob-Projects-a", "tail.jsonl"),
		[]byte(jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "g4", 0, 0, 0, 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "-Users-bob-Projects-a", "empty.jsonl"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(root, "-Users-bob-Projects-a", "bin.jsonl"), []byte{0, 1, 2, '\n', 0xff}, 0o644)

	rep := collect(root, true, day("2026-09-01"), time.Time{})
	// good: opus g1 (0.075) + opus content:7 line (5 tok, valid) + opus g4 (0.075) + sonnet g3 (0.015)
	want := 0.075 + 5*75/1e6 + 0.075 + 0.015
	if !near(rep.TotalCost, want) {
		t.Fatalf("total %v want %v", rep.TotalCost, want)
	}
	if opus, _ := rowByKey(rep.ByModel, "opus"); opus.Tokens != 1000+5+1000 {
		t.Fatalf("oversized line leaked into totals: opus tokens %d", opus.Tokens)
	}
	if o, ok := rowByKey(rep.ByModel, "other"); !ok || o.Cost != 0 || o.Tokens != 1000 {
		t.Fatalf("unknown model row: %+v", o)
	}
}

func TestBurnReport_ProjectLabel(t *testing.T) {
	cases := map[string]string{
		"-Users-bob-Projects-alpha":        "alpha",
		"-Users-bob-Projects-my-cool-repo": "my-cool-repo",
		"-Users-bob-Projects":              "Projects",
		"-Users-bob-scratch":               "scratch",
		"-Users-bob-Projects-a--claude-wt": "a--claude-wt",
		"weird_name":                       "weird_name",
		"-opt-build-thing":                 "-opt-build-thing",
		"":                                 "(unknown)",
	}
	for in, want := range cases {
		if got := projectLabel(in); got != want {
			t.Errorf("projectLabel(%q) = %q want %q", in, got, want)
		}
	}
}

func TestBurnReport_NestedTranscriptRollsUpToProject(t *testing.T) {
	root := t.TempDir()
	writeT(t, root, "-Users-bob-Projects-alpha/sess/subagents", "x.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-haiku-4-5", "n1", 0, 0, 0, 1000))
	rep := collect(root, false, day("2026-09-01"), time.Time{})
	if _, ok := rowByKey(rep.ByProject, "alpha"); !ok || len(rep.ByProject) != 1 {
		t.Fatalf("projects: %+v", rep.ByProject)
	}
}

func TestBurnReport_ToolMixDedupByToolUseID(t *testing.T) {
	line := func(id, toolID, name string) usageRecord {
		return usageRecord{Project: "p", Day: "2026-09-24", Model: "claude-opus-4-8", MsgID: id, Output: 1,
			Tools: []toolCall{{ID: toolID, Name: name}}}
	}
	rep := Aggregate(seq(
		line("m1", "t1", "Bash"),
		line("m1", "t2", "Task"), // same message, different block: counts
		line("m1", "t2", "Task"), // verbatim repeat: does not
		line("m1", "t1", "Bash"), // verbatim repeat: does not
	), DefaultPrices, day("2026-09-01"), time.Time{}, true)
	if len(rep.ToolMix) != 1 || rep.ToolMix[0].Calls != 2 || !near(rep.ToolMix[0].ICPct, 50) {
		t.Fatalf("mix = %+v", rep.ToolMix)
	}
}

func TestWalkTranscriptsBounded_MaxFilesTruncates(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeT(t, root, "-Users-bob-Projects-a", fmt.Sprintf("s%d.jsonl", i),
			jl("2026-09-24T10:00:00Z", "claude-opus-4-8", fmt.Sprintf("m%d", i), 0, 0, 0, 1000))
	}
	seqr, trunc := WalkTranscriptsBounded(root, false, Bounds{MaxFiles: 2})
	rep := Aggregate(seqr, DefaultPrices, day("2026-09-01"), time.Time{}, false)
	if !trunc() {
		t.Fatal("expected truncated=true when file cap hit")
	}
	if opus, _ := rowByKey(rep.ByModel, "opus"); opus.Tokens != 2000 {
		t.Fatalf("cap not honored: %+v", rep.ByModel)
	}
	_, trunc2 := WalkTranscriptsBounded(root, false, Bounds{MaxFiles: 50})
	if trunc2() {
		t.Fatal("unconsumed/uncapped walk must not report truncated")
	}
}

func TestWalkTranscriptsBounded_ModifiedSincePrunesOldFiles(t *testing.T) {
	root := t.TempDir()
	writeT(t, root, "-Users-bob-Projects-a", "old.jsonl", jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "o1", 0, 0, 0, 1000))
	writeT(t, root, "-Users-bob-Projects-a", "new.jsonl", jl("2026-09-24T10:00:00Z", "claude-sonnet-5-5", "n1", 0, 0, 0, 1000))
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "-Users-bob-Projects-a", "old.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	seqr, _ := WalkTranscriptsBounded(root, false, Bounds{ModifiedSince: time.Now().Add(-24 * time.Hour)})
	rep := Aggregate(seqr, DefaultPrices, day("2026-09-01"), time.Time{}, false)
	if _, ok := rowByKey(rep.ByModel, "opus"); ok || len(rep.ByModel) != 1 {
		t.Fatalf("stale file not pruned: %+v", rep.ByModel)
	}
}

// Row order is part of the report and is built from maps, whose iteration order is random. The fixture
// has many rows whose cost order, name order and day order disagree, and it is aggregated repeatedly, so
// removing a sort fails every run rather than some.
func TestBurnReport_RowOrderDoesNotDependOnMapIteration(t *testing.T) {
	var recs []usageRecord
	// 12 projects whose costs are all different and whose names sort the OTHER way from their cost, so
	// "by cost, descending" and "by name" disagree; 12 days; 3 model families of different cost.
	for i := 0; i < 12; i++ {
		recs = append(recs, usageRecord{
			Project: fmt.Sprintf("proj-%02d", i),
			Day:     fmt.Sprintf("2026-09-%02d", 10+(11-i)), // day order opposite to project order
			Model:   "claude-opus-4-8",
			Output:  int64(1_000_000 * (i + 1)),
		})
	}
	recs = append(recs,
		usageRecord{Project: "proj-00", Day: "2026-09-10", Model: "claude-sonnet-5-5", Output: 3_000_000},
		usageRecord{Project: "proj-00", Day: "2026-09-10", Model: "claude-haiku-4-5", Output: 1_000_000})

	for run := 0; run < 64; run++ {
		rep := Aggregate(seq(recs...), DefaultPrices, day("2026-09-01"), time.Time{}, false)

		var proj []string
		for _, r := range rep.ByProject {
			proj = append(proj, r.Key)
		}
		// project cost rises with i, so descending cost is proj-11 ... proj-00 (proj-00 also carries the
		// sonnet and haiku rows, which are cheaper than proj-11..proj-01's opus)
		if got := strings.Join(proj, ","); !strings.HasPrefix(got, "proj-11,proj-10,proj-09,proj-08,proj-07") {
			t.Fatalf("run %d: by-project must be cost descending: %s", run, got)
		}
		for i := 1; i < len(rep.ByProject); i++ {
			if rep.ByProject[i-1].Cost < rep.ByProject[i].Cost {
				t.Fatalf("run %d: by-project not sorted by cost descending: %+v", run, rep.ByProject)
			}
		}
		var days []string
		for _, r := range rep.ByDay {
			days = append(days, r.Key)
		}
		if !sort.StringsAreSorted(days) || len(days) != 12 {
			t.Fatalf("run %d: by-day must be chronological: %v", run, days)
		}
		var models []string
		for _, r := range rep.ByModel {
			models = append(models, r.Key)
		}
		if got := strings.Join(models, ","); got != "opus,sonnet,haiku" {
			t.Fatalf("run %d: by-model must be cost descending: %s", run, got)
		}
	}
}
