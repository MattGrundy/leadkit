// ACCOUNT mode (docs/build-tasks/multi-project-board.md): aggregates EVERY
// registered project's queue + hierarchy into one board — an index/switcher
// plus, per project, the SAME rendering single-project mode already gives one
// project (Scan, ScanStore, BuildTree, every template in templates.go). This
// file is a router + a per-project loop around that existing machinery, not a
// second implementation of it: handleProject below builds a plain *Server
// (the exact type single-project serve uses) from the registered roots and
// hands the request straight to its own Routes(), so /p/<slug>/tree, /p/<slug>/
// /p/<slug>/store, /p/<slug>/health, and /p/<slug>/task all come along for
// free, byte-for-byte the same as visiting that project alone.
//
// HARD CONSTRAINT (see viewer.go's Server / ListenAndServe): single-project
// mode never imports this file's usage.* registry calls — AccountServer is a
// wholly separate type from Server, so a caller that never constructs an
// AccountServer never touches the project registry, full stop.
package viewer

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/usage"
)

// DefaultProjectStaleTTL is how old a registered project's UpdatedAt can be
// before the index flags it stale — mirrors burn's own registry TTL posture
// (a crashed/idle project's last-registered roots shouldn't be presented as
// fresh forever), but this is display-only: a stale project is still shown
// (greyed, per the spec), never omitted outright.
const DefaultProjectStaleTTL = 24 * time.Hour

// AccountServer is the read-only HTTP handler over the account-wide project
// registry — ACCOUNT mode's entire surface. ProjectsDir is required (usually
// usage.DefaultProjectsDir); StaleTTL defaults to DefaultProjectStaleTTL when
// zero.
type AccountServer struct {
	ProjectsDir string
	StaleTTL    time.Duration
}

func (a *AccountServer) staleTTL() time.Duration {
	if a.StaleTTL > 0 {
		return a.StaleTTL
	}
	return DefaultProjectStaleTTL
}

// accountProjectRow is one index row — every field the template needs,
// already resolved (never re-decides availability at render time).
type accountProjectRow struct {
	Slug, Name                       string
	QueueRoot, StoreRoot, IssuesRoot string
	UpdatedAt                        time.Time
	Stale                            bool
	Available                        bool
	Unavailable                      string // reason shown when !Available
	TaskTotal                        int
	Href                             string
}

type accountIndexData struct {
	ProjectsDir string
	Projects    []accountProjectRow
}

// projectRows reads the registry and resolves each entry's live availability +
// task count — best-effort, PER PROJECT: one project's unreadable root never
// stops the others from resolving (multi-project-board.md's fault-isolation
// requirement). A registry read failure (not "missing", a real error) or an
// empty registry both just yield an empty slice — never a panic/500, the
// index template already renders that as "no projects registered".
func (a *AccountServer) projectRows() []accountProjectRow {
	entries, _ := usage.ReadProjectRegistry(a.ProjectsDir)
	now := time.Now()
	rows := make([]accountProjectRow, 0, len(entries))
	for _, e := range entries {
		row := accountProjectRow{
			Slug: e.Slug, Name: e.Name,
			QueueRoot: e.QueueRoot, StoreRoot: e.StoreRoot, IssuesRoot: e.IssuesRoot,
			UpdatedAt: e.UpdatedAt,
			Stale:     !e.UpdatedAt.IsZero() && now.Sub(e.UpdatedAt) > a.staleTTL(),
			Href:      "/p/" + e.Slug + "/",
		}
		if row.Name == "" {
			row.Name = e.Slug
		}
		if fi, err := os.Stat(e.QueueRoot); err != nil || !fi.IsDir() {
			row.Available = false
			if e.QueueRoot == "" {
				row.Unavailable = "no queue root registered"
			} else if err != nil {
				row.Unavailable = "queue root unreadable: " + err.Error()
			} else {
				row.Unavailable = "queue root is not a directory: " + e.QueueRoot
			}
			rows = append(rows, row)
			continue
		}
		row.Available = true
		tasks, _ := Scan(e.QueueRoot) // best-effort, same as single-project handleBoard
		row.TaskTotal = len(tasks)
		rows = append(rows, row)
	}
	return rows
}

// project looks up one registered entry by slug — "" ok means not found (a
// stale link, a typo, or a project that was never registered).
func (a *AccountServer) project(slug string) (usage.ProjectEntry, bool) {
	entries, _ := usage.ReadProjectRegistry(a.ProjectsDir)
	for _, e := range entries {
		if e.Slug == slug {
			return e, true
		}
	}
	return usage.ProjectEntry{}, false
}

func (a *AccountServer) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/p/", a.handleProject)
	return mux
}

func (a *AccountServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	rows := a.projectRows()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Slug < rows[j].Slug })
	data := accountIndexData{ProjectsDir: a.ProjectsDir, Projects: rows}
	if err := accountIndexTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleProject dispatches /p/<slug>/... to a plain per-project *Server built
// from that slug's registered roots — the SAME Server single-project serve
// uses, so every route it exposes (/, /tree, /store, /task, /health) renders
// with the exact existing templates/logic, no reimplementation. A slug the
// registry doesn't know about (never registered, or since removed) 404s
// rather than crashing the rest of the board.
func (a *AccountServer) handleProject(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/p/")
	slug, sub, _ := strings.Cut(rest, "/")
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	entry, ok := a.project(slug)
	if !ok {
		http.Error(w, "leadkit: unknown project "+slug, 404)
		return
	}

	// Reroute the request onto the sub-path a per-project Server expects
	// ("/p/<slug>/tree" -> "/tree"), preserving method/query/body untouched.
	sub2 := "/" + sub
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Path = sub2
	u.RawPath = ""
	r2.URL = &u

	s := &Server{Root: entry.QueueRoot, Store: entry.StoreRoot, IssuesRoot: entry.IssuesRoot}
	s.Routes().ServeHTTP(w, r2)
}

// ListenAndServeAccount starts the account-mode board.
func ListenAndServeAccount(a *AccountServer, addr string) error {
	fmt.Printf("leadkit viewer: serving ACCOUNT mode (projects=%s) at http://%s (read-only)\n", a.ProjectsDir, addr)
	return http.ListenAndServe(addr, a.Routes())
}

var accountIndexTmpl = template.Must(template.New("account-index").Parse(`<!doctype html><html><head>
<meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>leadkit account board</title><style>` + baseCSS + accountCSS + `</style></head><body>
<header><h1>leadkit account board</h1>
<span class=muted>{{len .Projects}} project(s) registered</span>
</header>
<div class=wrap>
{{if not .Projects}}<p class=muted>no projects registered — run leadkit burn/serve in a project</p>{{else}}
<table class=meta>
<tr><td class=k>project</td><td class=k>slug</td><td class=k>tasks</td><td class=k>updated</td></tr>
{{range .Projects}}<tr>
<td>{{if .Available}}<a href="{{.Href}}">{{.Name}}</a>{{else}}<span class=muted>{{.Name}}</span>{{end}}</td>
<td class=muted>{{.Slug}}</td>
<td>{{if .Available}}{{.TaskTotal}}{{else}}<span class="pill unavailable">unavailable</span>{{end}}</td>
<td class=muted>{{.UpdatedAt.Format "2006-01-02 15:04"}}{{if .Stale}} <span class="pill stale">stale</span>{{end}}</td>
</tr>{{if not .Available}}<tr><td colspan=4 class=muted>{{.Unavailable}}</td></tr>{{end}}{{end}}
</table>
{{end}}
</div></body></html>`))

const accountCSS = `
.pill.unavailable{background:#4a2a1f;color:#ffb38a}
.pill.stale{background:#3a3320;color:#e0c877}
`
