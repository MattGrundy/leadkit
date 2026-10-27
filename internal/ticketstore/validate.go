package ticketstore

import (
	"fmt"
	"strings"
)

// scalarKeys are the frontmatter keys read as one line of text; any other form for them is a finding.
// `blocked_on` is a scalar here: a list form is refused on write.
var scalarKeys = map[string]bool{
	"id": true, "title": true, "status": true, "type": true, "priority": true,
	"created": true, "updated": true, "blocked_on": true, "branch": true, "pr": true,
	"sha": true, "repo": true, "stage": true, "gate": true,
}

// mapKeys are the keys whose value is a nested map.
var mapKeys = map[string]bool{"trackers": true}

// requiredKeys must be present with a value for a ticket to be well formed.
var requiredKeys = []string{"id", "title", "status"}

// validate appends the content findings: a field of the wrong shape, a missing required field and
// a non-numeric id. The parse-level findings are already on the document.
func (d *Document) validate() {
	for _, f := range d.Fields {
		if f.Kind == KindFiller {
			continue
		}
		switch {
		case scalarKeys[f.Key] && !(f.Kind == KindScalar && canonicalScalar(f.Style)):
			d.add(f.Line, CodeNonCanonical, fmt.Sprintf("key %q is %s, which is not allowed for it; write it as a single-line value", f.Key, describe(f)))
		case mapKeys[f.Key] && f.Kind != KindMap:
			d.add(f.Line, CodeNonCanonical, fmt.Sprintf("key %q must be a map, not %s", f.Key, describe(f)))
		}
	}
	for _, k := range requiredKeys {
		if d.Value(k) == "" {
			d.add(0, CodeMissingField, fmt.Sprintf("required field %q is missing or empty", k))
		}
	}
	if id := d.Value("id"); id != "" && !allDigits(id) {
		d.add(0, CodeBadID, fmt.Sprintf("id %q is not a number", id))
	}
}

func canonicalScalar(s Style) bool { return s == StylePlain || s == StyleDouble || s == StyleSingle }

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func describe(f Field) string {
	switch {
	case f.Style == StyleFolded:
		return "a folded block scalar (`>`)"
	case f.Style == StyleLiteral:
		return "a literal block scalar (`|`)"
	case f.Style == StyleMultiline:
		return "a value continued over several lines"
	case f.Kind == KindList && f.Style == StyleFlow:
		return "a flow list"
	case f.Kind == KindList:
		return "a block list"
	case f.Kind == KindMap && f.Style == StyleFlow:
		return "a flow map"
	case f.Kind == KindMap:
		return "a nested map"
	}
	return "an unsupported form"
}

// Validate returns the findings that stop this document being written: every finding the parse
// produced. A store-wide finding, such as a duplicate id, is not about one document and is
// reported by Load instead; it never blocks a write, because the path is the key and a ticket
// with a clashing id is still a ticket that can be edited.
func (d *Document) Validate() []Finding { return d.Findings }

// summarize joins findings into one error message.
func summarize(fs []Finding) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.String()
	}
	return strings.Join(parts, "; ")
}
