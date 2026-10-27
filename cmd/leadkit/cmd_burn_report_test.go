package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burnreport"
)

func day(s string) time.Time { t, _ := time.Parse(time.DateOnly, s); return t }

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

func collect(root string, tools bool, since, until time.Time) burnreport.Report {
	return burnreport.Aggregate(burnreport.WalkTranscripts(root, tools), burnreport.DefaultPrices, since, until, tools)
}

func TestBurnReport_JSONSchemaStable(t *testing.T) {
	root := t.TempDir()
	writeT(t, root, "-Users-bob-Projects-alpha", "s.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "j1", 0, 0, 0, 1000, "Bash"))
	rep := collect(root, true, day("2026-09-01"), day("2026-09-30"))
	var buf bytes.Buffer
	if err := renderBurnReport(&buf, rep, burnReportOpts{json: true}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not valid json: %v", err)
	}
	for _, k := range []string{"since", "until", "est_usd_total", "tokens_total", "caveat", "by_model", "by_project", "by_day", "opus_tool_mix"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, buf.String())
		}
	}
	row := got["by_project"].([]any)[0].(map[string]any)
	for _, k := range []string{"key", "est_usd", "tokens", "pct_of_total", "opus_pct"} {
		if _, ok := row[k]; !ok {
			t.Errorf("missing row key %q", k)
		}
	}
	mix := got["opus_tool_mix"].([]any)[0].(map[string]any)
	for _, k := range []string{"project", "opus_tool_calls", "ic_pct", "orch_pct"} {
		if _, ok := mix[k]; !ok {
			t.Errorf("missing mix key %q", k)
		}
	}
}

func TestBurnReport_HumanOutput(t *testing.T) {
	root := t.TempDir()
	writeT(t, root, "-Users-bob-Projects-alpha", "s.jsonl",
		jl("2026-09-24T10:00:00Z", "claude-opus-4-8", "h1", 0, 0, 0, 1000, "Bash"))
	rep := collect(root, true, day("2026-09-01"), time.Time{})
	var buf bytes.Buffer
	_ = renderBurnReport(&buf, rep, burnReportOpts{tools: true})
	out := buf.String()
	for _, s := range []string{"since 2026-09-01 — est $0.07 (list-price proxy)", "not billed $", "BY MODEL", "BY PROJECT", "BY DAY", "OPUS TOOL-MIX", "alpha"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	buf.Reset()
	_ = renderBurnReport(&buf, rep, burnReportOpts{by: "day"})
	if strings.Contains(buf.String(), "BY MODEL") || !strings.Contains(buf.String(), "BY DAY") {
		t.Errorf("--by day rollup wrong:\n%s", buf.String())
	}

}

// Real transcripts put each content block of one message on its own line, all sharing
// message.id. Tool calls must be counted from EVERY such line (dedup by tool_use id, not
// message.id), while a line repeated verbatim is counted once.
