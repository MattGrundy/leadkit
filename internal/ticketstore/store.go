package ticketstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Ticket is one file of the store, read.
type Ticket struct {
	// Path is the key. Nothing else about a ticket is unique.
	Path string
	Doc  *Document

	ID        string // the frontmatter id; "" when it has none
	FileID    string // the number the file name starts with
	Title     string // folded and quoted forms decoded
	Status    string
	Type      string
	Priority  string
	BlockedOn []string // a scalar is one entry, a block or flow list is its items; none when empty
	Branch    string
	PR        string // the ticket's OWN top-level `pr`; a tracker's pr is Doc.Lookup("trackers", ...)

	// Findings are this file's own: its parse and shape problems, plus a duplicate id or an id
	// that disagrees with the file name once the whole store is known.
	Findings []Finding
}

// Loaded is a whole store read: every .md file, in path order, whether or not it read cleanly.
type Loaded struct {
	Dir      string
	Tickets  []*Ticket
	Findings []Finding // every ticket's findings, in path order
}

// Load reads every `*.md` file directly under dir. It returns an error only when the directory
// itself cannot be read. A file that is unreadable, has no frontmatter, or clashes with another
// file's id is a Ticket carrying a Finding, never an omission and never an error, so
// len(Tickets) is the number of .md files on disk.
func Load(dir string) (*Loaded, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("ticketstore: no ticket store at %s: %w", dir, err)
	}
	l := &Loaded{Dir: dir}
	byID := map[string][]*Ticket{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		t := ReadFile(filepath.Join(dir, name))
		l.Tickets = append(l.Tickets, t)
		if id := t.key(); id != "" {
			byID[id] = append(byID[id], t)
		}
	}
	for id, ts := range byID {
		if len(ts) < 2 {
			continue
		}
		for _, t := range ts {
			var others []string
			for _, o := range ts {
				if o != t {
					others = append(others, filepath.Base(o.Path))
				}
			}
			t.Findings = append(t.Findings, Finding{Path: t.Path, Code: CodeDuplicateID,
				Msg: fmt.Sprintf("id %s is also the id of %s", id, strings.Join(others, ", "))})
		}
	}
	for _, t := range l.Tickets {
		l.Findings = append(l.Findings, t.Findings...)
	}
	return l, nil
}

// key is the id a duplicate check groups on: the frontmatter id, else the file name's number.
func (t *Ticket) key() string {
	if t.ID != "" {
		return t.ID
	}
	return t.FileID
}

// ReadFile reads one ticket. It never fails: a file that cannot be read is a Ticket with an
// unreadable Finding and an empty Document.
func ReadFile(path string) *Ticket {
	t := &Ticket{Path: path}
	t.FileID, _, _ = strings.Cut(filepath.Base(path), "-")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Doc = &Document{Path: path}
		t.Findings = []Finding{{Path: path, Code: CodeUnreadable, Msg: err.Error()}}
		return t
	}
	d := Parse(path, string(data))
	t.Doc = d
	t.Findings = append([]Finding(nil), d.Findings...)
	t.ID = d.Value("id")
	t.Title = d.Value("title")
	t.Status = d.Value("status")
	t.Type = d.Value("type")
	t.Priority = d.Value("priority")
	t.Branch = d.Value("branch")
	t.PR = d.Value("pr")
	t.BlockedOn = blockedOn(d)
	if t.ID != "" && allDigits(t.FileID) && t.ID != t.FileID {
		t.Findings = append(t.Findings, Finding{Path: path, Code: CodeIDMismatch,
			Msg: fmt.Sprintf("frontmatter id %s but the file name starts with %s", t.ID, t.FileID)})
	}
	sort.SliceStable(t.Findings, func(i, j int) bool { return t.Findings[i].Line < t.Findings[j].Line })
	return t
}

// blockedOn is the ticket's blockers as the file states them: one entry for a scalar, the items
// of a list, and none for an empty value.
func blockedOn(d *Document) []string {
	f, ok := d.Get("blocked_on")
	if !ok {
		return nil
	}
	var out []string
	switch f.Kind {
	case KindScalar:
		out = []string{f.Text}
	case KindList:
		out = f.Items
	}
	var kept []string
	for _, s := range out {
		if strings.TrimSpace(s) != "" {
			kept = append(kept, s)
		}
	}
	return kept
}
