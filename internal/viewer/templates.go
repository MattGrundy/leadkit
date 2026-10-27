package viewer

import (
	"html/template"

	"mgpp.dev/leadkit/internal/audit"
)

type column struct {
	Status    string
	Tasks     []Task
	Collapsed bool
}

// chip is one clickable board filter — a plain <a> so filtering works with no JS.
type chip struct {
	Label  string
	Href   string
	Active bool
}

type boardData struct {
	Root         string
	Lanes        []string
	LaneFilter   string
	StatusFilter string
	OwnerFilter  string
	HasFilter    bool // any of lane/status/owner is set — shows the "clear all" link
	Total        int
	Columns      []column
	HasStore     bool // queue board links to /store
	IsStore      bool // this render is the store view

	LaneChips   []chip
	StatusChips []chip
	OwnerChips  []chip
}

type taskData struct {
	Rel      string
	Src      string
	Lane     string
	Status   string
	Title    string
	Meta     map[string]string
	MetaKeys []string
	BodyHTML template.HTML
}

// backHref is where a task-detail page's "← tasks" link returns to.
func (t taskData) BackHref() string {
	if t.Src == "store" {
		return "/store"
	}
	return "/"
}

// healthFinding pairs an audit.Finding with its precomputed /task link (empty when
// the finding names no single file).
type healthFinding struct {
	Finding audit.Finding
	Href    string
}

type healthGroup struct {
	Category string
	Count    int
	Findings []healthFinding
}

type sessionCountRow struct {
	Name   string
	Counts audit.SessionCounts
}

type healthData struct {
	Summary        string // the one-line totals, matching `leadkit audit`'s format
	Groups         []healthGroup
	SessionCounts  []sessionCountRow
	HasStore       bool
	PushViolations []pushViolation // most-recent-first, from push-guard's deny log
	SweepStatus    sweepStatus     // the sweep's findings artifact (self-surfacing diagnostics)
}

const baseCSS = `
body{font:14px/1.5 system-ui,sans-serif;margin:0;background:#0f1115;color:#d7dae0}
a{color:#6ea8fe;text-decoration:none} a:hover{text-decoration:underline}
header{padding:12px 18px;border-bottom:1px solid #232733;display:flex;gap:14px;align-items:baseline;flex-wrap:wrap}
header h1{font-size:16px;margin:0}
.muted{color:#8b93a1;font-size:12px}
.board{display:flex;gap:14px;padding:16px;overflow-x:auto;align-items:flex-start}
.col{flex:0 0 300px;background:#161922;border:1px solid #232733;border-radius:8px}
.col h2{font-size:12px;text-transform:uppercase;letter-spacing:.05em;margin:0;padding:10px 12px;border-bottom:1px solid #232733;color:#aeb6c2}
.col .count{color:#6b7280}
.card{padding:9px 12px;border-bottom:1px solid #1d212b}
.card:last-child{border-bottom:none}
.card .title{font-weight:600}
.card .lane{font-size:11px;color:#8b93a1}
.pill{display:inline-block;font-size:11px;padding:1px 7px;border-radius:10px;background:#232733;color:#aeb6c2;margin-left:6px}
.wrap{max-width:900px;margin:0 auto;padding:18px}
table.meta{border-collapse:collapse;margin:10px 0;font-size:13px}
table.meta td{border:1px solid #232733;padding:4px 10px;vertical-align:top}
table.meta td.k{color:#8b93a1;white-space:nowrap}
.md h1{font-size:20px} .md h2{font-size:16px} .md h3{font-size:14px}
.md code{background:#232733;padding:1px 5px;border-radius:4px}
.md pre.code{background:#0b0d12;border:1px solid #232733;border-radius:6px;padding:10px;overflow-x:auto;white-space:pre}
.md br{line-height:0.6}
.chipbar{padding:8px 18px;border-bottom:1px solid #232733;display:flex;gap:10px;flex-wrap:wrap;align-items:baseline;font-size:12px}
.chipbar .dim{color:#8b93a1;margin-right:2px}
.chip{display:inline-block;font-size:11px;padding:2px 9px;border-radius:10px;background:#191d26;border:1px solid #232733;color:#aeb6c2;margin-right:4px}
.chip:hover{text-decoration:none;border-color:#3a4256}
.chip.active{background:#28406b;border-color:#3a5da8;color:#dce7ff}
.health-strip{padding:10px 18px;border-bottom:1px solid #232733;font-size:13px}
.hgroup{max-width:1000px;margin:0 auto;padding:14px 18px}
.hgroup h2{font-size:13px;text-transform:uppercase;letter-spacing:.05em;color:#aeb6c2;margin:18px 0 6px}
.hfinding{padding:6px 0;border-bottom:1px solid #1d212b;font-size:13px}
.hfinding .sev{display:inline-block;font-size:10px;padding:1px 6px;border-radius:8px;background:#232733;color:#aeb6c2;margin-right:6px;text-transform:uppercase}
.hfinding .sev.warn{background:#4a2a1f;color:#ffb38a}
.hfinding .sev.info{background:#1f3a4a;color:#8ad2ff}
.tree{max-width:1100px;margin:0 auto;padding:16px 18px}
.tree details{margin:2px 0 2px 16px;border-left:1px solid #232733;padding-left:10px}
.tree summary{cursor:pointer;padding:4px 0;list-style:none}
.tree summary::-webkit-details-marker{display:none}
.tree summary::before{content:"▸";display:inline-block;width:1em;color:#6b7280}
.tree details[open]>summary::before{content:"▾"}
.tree .node-label{font-weight:600}
.tree .rollup{color:#8b93a1;font-size:12px;margin-left:6px}
.tree .rollup .done{color:#6fcf97}
.tree .taskrow{padding:3px 0 3px 26px;font-size:13px;border-left:1px solid #232733;margin-left:16px}
.tree .taskrow .lane{color:#8b93a1;font-size:11px;margin-left:6px}
.tree .orphan{border-left-color:#4a2a1f}
.tree .orphan>summary::before{color:#ffb38a}
.tree .empty{color:#8b93a1;padding:12px 0}
`

var boardTmpl = template.Must(template.New("board").Parse(`<!doctype html><html><head>
<meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>leadkit tasks</title><style>` + baseCSS + `</style></head><body>
<header><h1>{{if .IsStore}}leadkit store{{else}}leadkit tasks{{end}}</h1>
<span class=muted>{{.Root}}</span>
<span class=muted>· {{.Total}} tasks</span>
{{if .IsStore}}<span class=muted>· <a href="/">← live board</a></span>{{else if .HasStore}}<span class=muted>· <a href="/store">full store →</a></span>{{end}}
<span class=muted>· <a href="/health">health →</a></span>
<span class=muted>· <a href="/tree">tree →</a></span>
{{if .HasFilter}}<span class=muted>· <a href="/">clear filters</a></span>{{end}}
</header>
{{if or .LaneChips .StatusChips .OwnerChips}}<div class=chipbar>
{{if .LaneChips}}<span class=dim>lane:</span>{{range .LaneChips}}<a class="chip{{if .Active}} active{{end}}" href="{{.Href}}">{{.Label}}</a>{{end}}{{end}}
{{if .StatusChips}}<span class=dim>status:</span>{{range .StatusChips}}<a class="chip{{if .Active}} active{{end}}" href="{{.Href}}">{{.Label}}</a>{{end}}{{end}}
{{if .OwnerChips}}<span class=dim>owner:</span>{{range .OwnerChips}}<a class="chip{{if .Active}} active{{end}}" href="{{.Href}}">{{.Label}}</a>{{end}}{{end}}
</div>{{end}}
<div class=board>
{{range .Columns}}<div class=col>
{{if .Collapsed}}<details><summary><h2 style="display:inline">{{.Status}} <span class=count>{{len .Tasks}}</span></h2></summary>
{{range .Tasks}}<div class=card>
<div class=title><a href="/task?p={{.Rel}}{{if .Src}}&src={{.Src}}{{end}}">{{.Title}}</a></div>
<div class=lane>{{.Lane}}{{if .Owner}}<span class=pill>{{.Owner}}</span>{{end}}</div>
</div>{{end}}
</details>
{{else}}<h2>{{.Status}} <span class=count>{{len .Tasks}}</span></h2>
{{range .Tasks}}<div class=card>
<div class=title><a href="/task?p={{.Rel}}{{if .Src}}&src={{.Src}}{{end}}">{{.Title}}</a></div>
<div class=lane><a href="/?lane={{.Lane}}">{{.Lane}}</a>{{if .Owner}}<span class=pill>{{.Owner}}</span>{{end}}</div>
</div>{{end}}
{{end}}
</div>{{end}}
</div></body></html>`))

var taskTmpl = template.Must(template.New("task").Parse(`<!doctype html><html><head>
<meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>` + baseCSS + `</style></head><body>
<header><h1><a href="{{.BackHref}}">← tasks</a></h1><span class=muted>{{.Lane}} · {{.Status}}</span></header>
<div class=wrap>
<h1>{{.Title}}</h1>
{{if .MetaKeys}}<table class=meta>
{{range .MetaKeys}}<tr><td class=k>{{.}}</td><td>{{index $.Meta .}}</td></tr>{{end}}
</table>{{end}}
<div class=md>{{.BodyHTML}}</div>
</div></body></html>`))

var healthTmpl = template.Must(template.New("health").Parse(`<!doctype html><html><head>
<meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>leadkit health</title><style>` + baseCSS + `</style></head><body>
<header><h1>leadkit health</h1>
<span class=muted>· <a href="/">← board</a></span>
{{if .HasStore}}<span class=muted>· <a href="/store">store →</a></span>{{end}}
</header>
<div class=health-strip>{{.Summary}}</div>
{{if .SweepStatus.Present}}<div class=hgroup>
<h2>daemon diagnostics <span class=count>{{.SweepStatus.Stalled}} stalled · {{.SweepStatus.Broken}} broken</span></h2>
<div class=muted>last sweep {{.SweepStatus.TS}}</div>
{{range .SweepStatus.BrokenLanes}}<div class=hfinding>
<span class="sev warn">broken</span> lane <b>{{.Lane}}</b> — dangling: {{range .Links}}{{.}} {{end}}<span class=muted>→ leadkit queue repair --apply</span>
</div>{{end}}
{{range .SweepStatus.StalledLanes}}<div class=hfinding>
<span class="sev warn">stalled</span> lane <b>{{.Lane}}</b> — {{.Pending}} pending, 0 in-progress <span class=muted>→ converge/assign or leadkit queue sweep --poke</span>
</div>{{end}}
{{if and (eq .SweepStatus.Stalled 0) (eq .SweepStatus.Broken 0)}}<div class=hfinding>no stalls, no broken links — queue healthy</div>{{end}}
</div>{{end}}
{{range .Groups}}<div class=hgroup>
<h2>{{.Category}} <span class=count>{{.Count}}</span></h2>
{{range .Findings}}<div class=hfinding>
<span class="sev {{.Finding.Severity}}">{{.Finding.Severity}}</span>
{{if .Href}}<a href="{{.Href}}">{{.Finding.ID}}</a>{{else}}<b>{{.Finding.ID}}</b>{{end}}
— {{.Finding.Detail}}
</div>{{end}}
</div>{{end}}
{{if .SessionCounts}}<div class=hgroup>
<h2>session counts</h2>
{{range .SessionCounts}}<div class=hfinding>{{.Name}}: total={{.Counts.Total}} gate={{.Counts.Gate}} misuse={{.Counts.Misuse}}</div>{{end}}
</div>{{end}}
{{if .PushViolations}}<div class=hgroup>
<h2>push violations <span class=count>{{len .PushViolations}}</span></h2>
{{range .PushViolations}}<div class=hfinding>
<span class="sev warn">{{.Reason}}</span>
{{.TS}} · repo <b>{{.Repo}}</b> · target <b>{{.Target}}</b>{{if .Force}} · force{{end}} · session {{.SessionID}}
<div class=muted>{{.Command}}</div>
</div>{{end}}
</div>{{end}}
</body></html>`))

var treeTmpl = template.Must(template.New("tree").Parse(`<!doctype html><html><head>
<meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>leadkit hierarchy</title><style>` + baseCSS + `</style></head><body>
<header><h1>leadkit hierarchy</h1>
<span class=muted>{{.Root}}</span>
{{if .Project}}<span class=muted>· {{.Project}}</span>{{end}}
<span class=muted>· <a href="/">← board</a></span>
</header>
<div class=tree>
{{if not .HasHierarchy}}<div class=empty>no hierarchy seeded yet — missing/empty milestones.yaml and issues/. The queue board above is unaffected.</div>{{end}}
{{range .Milestones}}<details open>
<summary><span class=node-label>{{.Title}}</span> <span class=muted>{{.ID}} · {{.State}}</span>{{template "rollup" .Counts}}</summary>
{{range .Epics}}<details>
<summary><span class=node-label>{{.ID}}</span>{{if .Note}} <span class=muted>{{.Note}}</span>{{end}}{{template "rollup" .Counts}}</summary>
{{range .Issues}}<details>
<summary><span class=node-label>{{.Title}}</span> <span class=muted>{{.ID}}</span>{{template "rollup" .Counts}}</summary>
{{range .Tasks}}{{template "taskrow" .}}{{else}}<div class="taskrow muted">no tasks</div>{{end}}
</details>{{end}}
{{range .DirectTasks}}{{template "taskrow" .}}{{end}}
</details>{{end}}
</details>{{end}}
{{if .OrphanIssues}}<details class=orphan>
<summary><span class=node-label>unparented / orphan issues</span> <span class=rollup>{{len .OrphanIssues}} issue(s)</span></summary>
{{range .OrphanIssues}}<details>
<summary><span class=node-label>{{.Title}}</span> <span class=muted>{{.ID}}</span>{{template "rollup" .Counts}}</summary>
{{range .Tasks}}{{template "taskrow" .}}{{else}}<div class="taskrow muted">no tasks</div>{{end}}
</details>{{end}}
</details>{{end}}
{{if .OrphanTasks}}<details class=orphan open>
<summary><span class=node-label>unparented / orphan tasks</span> <span class=rollup>{{len .OrphanTasks}} task(s)</span></summary>
{{range .OrphanTasks}}{{template "taskrow" .}}{{end}}
</details>{{end}}
</div>
{{define "rollup"}}<span class=rollup>{{.Total}} tasks · <span class=done>{{.Done}} done</span> · {{.Active}} active</span>{{end}}
{{define "taskrow"}}<div class=taskrow>{{.Title}} <span class=pill>{{.Status}}</span><span class=lane>{{.Lane}}</span></div>{{end}}
</body></html>`))
