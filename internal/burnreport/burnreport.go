// Package burnreport is the READ-ONLY spend aggregation over Claude Code session
// transcripts (<projects-dir>/<encoded-cwd>/**/<session>.jsonl, one JSON object per
// line). It is DISTINCT from internal/burn (the live throttle): transcripts carry
// per-message token usage + model — the durable ground truth for "where did burn go".
// Shared by `leadkit burn-report` (cmd/leadkit) and `leadkit doctor`'s subagent-model
// check. The $ figure is a token-weighted LIST-PRICE PROXY, not billed spend.
package burnreport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PriceTable maps a model family to $/1M tokens: [input, cache_write, cache_read, output].
type PriceTable map[string][4]float64

// DefaultPrices are the built-in list prices. Edit here to re-baseline (e.g. when a new
// model generation's pricing lands). Families are classified by substring in modelFamily.
var DefaultPrices = PriceTable{
	"opus":   {15, 18.75, 1.5, 75},
	"sonnet": {3, 3.75, 0.3, 15},
	"haiku":  {0.8, 1, 0.08, 4},
}

const (
	familyOpus   = "opus"
	familySonnet = "sonnet"
	familyHaiku  = "haiku"
	familyOther  = "other"

	// maxTranscriptLine caps one transcript line; a longer line is skipped, not fatal.
	maxTranscriptLine = 16 << 20
)

// icTools / orchTools drive the --tools lens: hands-on work vs delegation.
var (
	icTools   = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true, "Bash": true}
	orchTools = map[string]bool{"Task": true, "Agent": true, "SendMessage": true}
)

// modelFamily classifies a model string by substring.
func modelFamily(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, familyOpus):
		return familyOpus
	case strings.Contains(m, familySonnet):
		return familySonnet
	case strings.Contains(m, familyHaiku):
		return familyHaiku
	}
	return familyOther
}

// usageRecord is one parsed transcript line reduced to what the report needs.
type usageRecord struct {
	Project string
	Day     string // YYYY-MM-DD
	Model   string
	MsgID   string // message.id — a multi-block message repeats across lines with the same usage
	Input   int64
	CacheW  int64
	CacheR  int64
	Output  int64
	Tools   []toolCall
}

// toolCall is one tool_use block; ID (when present) dedupes a block repeated across lines/files.
type toolCall struct{ ID, Name string }

func (r usageRecord) tokens() int64 { return r.Input + r.CacheW + r.CacheR + r.Output }

// cost is the list-price proxy for one record.
func (p PriceTable) cost(family string, r usageRecord) float64 {
	pr, ok := p[family]
	if !ok {
		return 0
	}
	return (float64(r.Input)*pr[0] + float64(r.CacheW)*pr[1] + float64(r.CacheR)*pr[2] + float64(r.Output)*pr[3]) / 1e6
}

// Row is one aggregate line. OpusPct is only meaningful for project rows.
type Row struct {
	Key     string  `json:"key"`
	Cost    float64 `json:"est_usd"`
	Tokens  int64   `json:"tokens"`
	Pct     float64 `json:"pct_of_total"`
	OpusPct float64 `json:"opus_pct,omitempty"`
}

// ToolMix is the per-project Opus tool-call split.
type ToolMix struct {
	Project string  `json:"project"`
	Calls   int     `json:"opus_tool_calls"`
	ICPct   float64 `json:"ic_pct"`
	OrchPct float64 `json:"orch_pct"`
}

// Report is the stable, JSON-serializable result (the A/B diff contract).
type Report struct {
	Since     string    `json:"since"`
	Until     string    `json:"until,omitempty"`
	TotalCost float64   `json:"est_usd_total"`
	Tokens    int64     `json:"tokens_total"`
	Caveat    string    `json:"caveat"`
	ByModel   []Row     `json:"by_model"`
	ByProject []Row     `json:"by_project"`
	ByDay     []Row     `json:"by_day"`
	ToolMix   []ToolMix `json:"opus_tool_mix,omitempty"`
}

const reportCaveat = "estimated at list prices — a token-weighted cost proxy, not billed $"

type acc struct {
	cost, opusCost float64
	tokens         int64
}

// Aggregate folds the record stream into a Report. Days are compared as YYYY-MM-DD
// strings: since is inclusive; a non-zero until is inclusive of its whole day.
func Aggregate(records iter.Seq[usageRecord], prices PriceTable, since, until time.Time, wantTools bool) Report {
	sinceDay := since.UTC().Format(time.DateOnly)
	untilDay := ""
	if !until.IsZero() {
		untilDay = until.UTC().Format(time.DateOnly)
	}
	models, projects, days := map[string]*acc{}, map[string]*acc{}, map[string]*acc{}
	type mix struct{ calls, ic, orch int }
	mixes := map[string]*mix{}
	seen := map[string]bool{}
	seenTool := map[string]bool{}
	var total float64
	var totalTok int64

	bump := func(m map[string]*acc, k string, c float64, tok int64, isOpus bool) {
		a := m[k]
		if a == nil {
			a = &acc{}
			m[k] = a
		}
		a.cost += c
		a.tokens += tok
		if isOpus {
			a.opusCost += c
		}
	}

	for r := range records {
		if r.Day < sinceDay || (untilDay != "" && r.Day > untilDay) {
			continue
		}
		fam := modelFamily(r.Model)
		if wantTools && fam == familyOpus && len(r.Tools) > 0 {
			mx := mixes[r.Project]
			if mx == nil {
				mx = &mix{}
				mixes[r.Project] = mx
			}
			for _, t := range r.Tools {
				// Each content block of a multi-block message sits on its OWN line (same
				// message.id), so tool_use blocks must NOT be deduped by message.id — only
				// by their own tool_use id.
				if t.ID != "" {
					if seenTool[t.ID] {
						continue
					}
					seenTool[t.ID] = true
				}
				mx.calls++
				if icTools[t.Name] {
					mx.ic++
				} else if orchTools[t.Name] {
					mx.orch++
				}
			}
		}
		// A message split across several lines repeats identical usage on each; count once.
		if r.MsgID != "" {
			if seen[r.MsgID] {
				continue
			}
			seen[r.MsgID] = true
		}
		c := prices.cost(fam, r)
		tok := r.tokens()
		total += c
		totalTok += tok
		isOpus := fam == familyOpus
		bump(models, fam, c, tok, isOpus)
		bump(projects, r.Project, c, tok, isOpus)
		bump(days, r.Day, c, tok, isOpus)
	}

	rep := Report{Since: sinceDay, Until: untilDay, TotalCost: total, Tokens: totalTok, Caveat: reportCaveat}
	rows := func(m map[string]*acc, byKey bool) []Row {
		out := make([]Row, 0, len(m))
		for k, a := range m {
			row := Row{Key: k, Cost: a.cost, Tokens: a.tokens}
			if total > 0 {
				row.Pct = a.cost / total * 100
			}
			if a.cost > 0 {
				row.OpusPct = a.opusCost / a.cost * 100
			}
			out = append(out, row)
		}
		sort.Slice(out, func(i, j int) bool {
			if byKey {
				return out[i].Key < out[j].Key
			}
			if out[i].Cost != out[j].Cost {
				return out[i].Cost > out[j].Cost
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	rep.ByModel = rows(models, false)
	rep.ByProject = rows(projects, false)
	rep.ByDay = rows(days, true)
	for p, mx := range mixes {
		if mx.calls == 0 {
			continue
		}
		rep.ToolMix = append(rep.ToolMix, ToolMix{
			Project: p, Calls: mx.calls,
			ICPct:   float64(mx.ic) / float64(mx.calls) * 100,
			OrchPct: float64(mx.orch) / float64(mx.calls) * 100,
		})
	}
	sort.Slice(rep.ToolMix, func(i, j int) bool {
		if rep.ToolMix[i].Calls != rep.ToolMix[j].Calls {
			return rep.ToolMix[i].Calls > rep.ToolMix[j].Calls
		}
		return rep.ToolMix[i].Project < rep.ToolMix[j].Project
	})
	return rep
}

var projectPrefixRE = regexp.MustCompile(`^-Users-[^-]+-(?:Projects-)?`)

// projectLabel turns Claude Code's encoded-cwd dir name into a readable slug. A name
// that doesn't match the /Users/<user>/… shape is returned unchanged (never empty).
func projectLabel(dir string) string {
	s := projectPrefixRE.ReplaceAllString(dir, "")
	if s == "" {
		if dir == "" {
			return "(unknown)"
		}
		return dir
	}
	return s
}

// transcriptLine is the defensive subset of a transcript line.
type transcriptLine struct {
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input  int64 `json:"input_tokens"`
			CacheW int64 `json:"cache_creation_input_tokens"`
			CacheR int64 `json:"cache_read_input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var usageKey = []byte(`"usage"`)

// parseLine reduces one transcript line to a usageRecord; ok=false means skip
// (malformed, or missing message/usage/timestamp/model).
func parseLine(line []byte, project string, wantTools bool) (usageRecord, bool) {
	// Cheap pre-filter: a line without a usage key can never yield a record, so skip the
	// (expensive) full JSON decode — most transcript bytes are tool output/content.
	if !bytes.Contains(line, usageKey) {
		return usageRecord{}, false
	}
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return usageRecord{}, false
	}
	if tl.Message == nil || tl.Message.Usage == nil || tl.Message.Model == "" || len(tl.Timestamp) < 10 {
		return usageRecord{}, false
	}
	day := tl.Timestamp[:10]
	if _, err := time.Parse(time.DateOnly, day); err != nil {
		return usageRecord{}, false
	}
	u := tl.Message.Usage
	rec := usageRecord{
		Project: project, Day: day, Model: tl.Message.Model, MsgID: tl.Message.ID,
		Input: u.Input, CacheW: u.CacheW, CacheR: u.CacheR, Output: u.Output,
	}
	if wantTools && len(tl.Message.Content) > 0 && tl.Message.Content[0] == '[' {
		var blocks []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(tl.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "tool_use" && b.Name != "" {
					rec.Tools = append(rec.Tools, toolCall{ID: b.ID, Name: b.Name})
				}
			}
		}
	}
	return rec, true
}

// scanTranscript streams one file line by line. An oversized line (bufio.ErrTooLong)
// is skipped by draining to its newline; the rest of the file still counts.
func scanTranscript(r io.Reader, project string, wantTools bool, yield func(usageRecord) bool) bool {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readCappedLine(br, maxTranscriptLine)
		if len(line) > 0 {
			if rec, ok := parseLine(line, project, wantTools); ok && !yield(rec) {
				return false
			}
		}
		if err != nil {
			return true
		}
	}
}

// readCappedLine reads one line up to limit bytes; longer lines are drained and
// returned empty (skipped). err is non-nil at EOF/read error.
func readCappedLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > limit {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, err
		}
		return buf, err
	}
}

// WalkTranscripts yields records from every *.jsonl under projectsDir. The project
// label comes from the first path component under projectsDir (the encoded cwd), so
// nested subagent transcripts roll up to their project. Unreadable files are skipped.
func WalkTranscripts(projectsDir string, wantTools bool) iter.Seq[usageRecord] {
	seq, _ := walk(projectsDir, wantTools, Bounds{})
	return seq
}

// Bounds caps a transcript walk so a caller on a latency budget (doctor) stays snappy.
// Zero values mean unbounded.
type Bounds struct {
	ModifiedSince time.Time     // skip files whose mtime is older (they cannot hold newer records)
	MaxFiles      int           // stop after opening this many transcript files
	Deadline      time.Duration // stop once the walk has run this long
}

// Truncated reports, after the iterator has been consumed, whether a Bounds cap cut
// the walk short (so the totals are partial).
type Truncated func() bool

// WalkTranscriptsBounded is WalkTranscripts under Bounds. The returned Truncated is only
// meaningful once the sequence has been fully consumed.
func WalkTranscriptsBounded(projectsDir string, wantTools bool, b Bounds) (iter.Seq[usageRecord], Truncated) {
	return walk(projectsDir, wantTools, b)
}

func walk(projectsDir string, wantTools bool, b Bounds) (iter.Seq[usageRecord], Truncated) {
	truncated := false
	seq := func(yield func(usageRecord) bool) {
		stop := false
		files := 0
		start := time.Now()
		_ = filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
			if stop {
				return filepath.SkipAll
			}
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if b.Deadline > 0 && time.Since(start) > b.Deadline {
				truncated = true
				return filepath.SkipAll
			}
			if !b.ModifiedSince.IsZero() {
				if info, ierr := d.Info(); ierr == nil && info.ModTime().Before(b.ModifiedSince) {
					return nil
				}
			}
			if b.MaxFiles > 0 && files >= b.MaxFiles {
				truncated = true
				return filepath.SkipAll
			}
			rel, rerr := filepath.Rel(projectsDir, path)
			if rerr != nil {
				return nil
			}
			top, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			if !strings.Contains(filepath.ToSlash(rel), "/") {
				top = ""
			}
			f, oerr := os.Open(path)
			if oerr != nil {
				return nil
			}
			defer f.Close()
			files++
			if !scanTranscript(f, projectLabel(top), wantTools, yield) {
				stop = true
			}
			return nil
		})
	}
	return seq, func() bool { return truncated }
}
