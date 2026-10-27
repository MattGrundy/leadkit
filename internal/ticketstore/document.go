// Package ticketstore reads, validates and writes ticket files: markdown with a leading `---`
// frontmatter block.
//
// The reader reports in a Finding every form it does not understand; the validator allows only the
// canonical shape on the write path. The path is the key (a duplicate id is a Finding naming both
// files), a file with no frontmatter is still a Ticket carrying a Finding, and every write is a
// per-file compare-and-swap (see Update). The package depends on the filesystem only.
package ticketstore

import (
	"fmt"
	"strings"
)

// Kind is the shape of a field's value.
type Kind int

const (
	// KindScalar is a single string value, in any Style.
	KindScalar Kind = iota
	// KindList is a block or flow list of strings.
	KindList
	// KindMap is a nested block or flow map.
	KindMap
	// KindFiller is not a field: a blank or comment line between fields, kept so a write
	// reproduces the file byte for byte.
	KindFiller
)

// Style is how a value was written, which is what the canonical-form check keys on.
type Style int

const (
	StylePlain     Style = iota // key: value
	StyleDouble                 // key: "value"
	StyleSingle                 // key: 'value'
	StyleFolded                 // key: >
	StyleLiteral                // key: |
	StyleMultiline              // a plain scalar continued on indented lines
	StyleFlow                   // [a, b] or {k: v}
	StyleBlock                  // an indented block list or map
)

// Field is one top-level frontmatter entry.
type Field struct {
	Key      string
	Kind     Kind
	Style    Style
	Text     string   // KindScalar: the decoded value
	Items    []string // KindList: the decoded items
	Children []Field  // KindMap: the nested entries
	Line     int      // 1-based file line of the key

	// raw is the field's source lines, verbatim, so an untouched field is written back exactly.
	// It is nil for a field a write built.
	raw []string
}

// Finding is a problem with a ticket file, reported rather than raised.
type Finding struct {
	Path string
	Line int // 0 when the finding is about the file as a whole
	Code string
	Msg  string
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s: %s", f.Path, f.Line, f.Code, f.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", f.Path, f.Code, f.Msg)
}

// Finding codes. The first group is about the file's shape and blocks a write; the second is
// about the content and also blocks a write; the third is store-wide and does not.
const (
	CodeNoFrontmatter = "no-frontmatter"
	CodeUnclosed      = "unclosed-frontmatter"
	CodeBadLine       = "bad-line"
	CodeBadValue      = "bad-value"
	CodeDuplicateKey  = "duplicate-key"

	CodeNonCanonical = "noncanonical-form"
	CodeMissingField = "missing-field"
	CodeBadID        = "bad-id"

	CodeDuplicateID = "duplicate-id"
	CodeIDMismatch  = "id-mismatch"
	CodeUnreadable  = "unreadable"
)

// Document is a parsed ticket file: the frontmatter fields in order, and the body verbatim.
type Document struct {
	Path     string
	Fields   []Field
	Findings []Finding

	// HasFrontmatter is false when the file has no opening delimiter, or never closes it.
	HasFrontmatter bool

	open, close string // the delimiter lines as written
	body        string // everything after the closing delimiter's newline, verbatim
	bodyNL      bool   // whether the closing delimiter was followed by a newline at all
	eol         string
	src         string // the exact text parsed, returned by Render until something changes
	dirty       bool
}

// Get returns the first top-level field named key.
func (d *Document) Get(key string) (Field, bool) {
	for _, f := range d.Fields {
		if f.Kind != KindFiller && f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Value returns the decoded scalar for a top-level key, or "" when the key is absent or is not a
// scalar. A caller that must tell absent from empty uses Get.
func (d *Document) Value(key string) string {
	if f, ok := d.Get(key); ok && f.Kind == KindScalar {
		return f.Text
	}
	return ""
}

// Lookup walks a path of nested keys, `Lookup("trackers", "github", "pr")`, and returns the scalar
// at its end. The first element is a top-level key, so Lookup("pr") is the ticket's own `pr` and
// Lookup("trackers", "github", "pr") is a different thing entirely.
func (d *Document) Lookup(path ...string) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	f, ok := d.Get(path[0])
	if !ok {
		return "", false
	}
	for _, k := range path[1:] {
		if f.Kind != KindMap {
			return "", false
		}
		next, found := Field{}, false
		for _, c := range f.Children {
			if c.Key == k {
				next, found = c, true
				break
			}
		}
		if !found {
			return "", false
		}
		f = next
	}
	if f.Kind != KindScalar {
		return "", false
	}
	return f.Text, true
}

// Body is everything after the closing delimiter, verbatim.
func (d *Document) Body() string { return d.body }

// Render is the file's text. An unmodified Document renders exactly the bytes it was parsed from.
func (d *Document) Render() string {
	if !d.dirty {
		return d.src
	}
	var b strings.Builder
	b.WriteString(d.open)
	b.WriteString(d.eol)
	for _, f := range d.Fields {
		if f.raw != nil {
			for _, l := range f.raw {
				b.WriteString(l)
				b.WriteString(d.eol)
			}
			continue
		}
		for _, l := range renderField(f) {
			b.WriteString(l)
			b.WriteString(d.eol)
		}
	}
	b.WriteString(d.close)
	if d.bodyNL {
		b.WriteString(d.eol)
	}
	b.WriteString(d.body)
	return b.String()
}
