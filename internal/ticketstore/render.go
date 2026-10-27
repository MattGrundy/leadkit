package ticketstore

import (
	"fmt"
	"strings"
)

// renderField is the source lines for a field a write built or changed. Only scalars are ever
// written: a nested map or a list is read, validated and kept verbatim, never composed.
func renderField(f Field) []string {
	switch f.Kind {
	case KindScalar:
		return []string{f.Key + ": " + quoteScalar(f.Text)}
	case KindMap:
		if f.Style != StyleFlow {
			return nil
		}
		parts := make([]string, 0, len(f.Children))
		for _, c := range f.Children {
			parts = append(parts, c.Key+": "+quoteFlowScalar(c.Text))
		}
		return []string{f.Key + ": {" + strings.Join(parts, ", ") + "}"}
	case KindList:
		out := []string{f.Key + ":"}
		for _, it := range f.Items {
			out = append(out, "  - "+quoteScalar(it))
		}
		return out
	}
	return nil
}

// quoteScalar writes v the way it reads back unchanged: bare when that is unambiguous, otherwise
// double-quoted with only `\` and `"` escaped, the two escapes the reader resolves.
func quoteScalar(v string) string {
	if plainSafe(v) {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// quoteFlowScalar is quoteScalar for a value inside `{k: v, ...}`, where a comma or a bracket would
// end the entry and so forces quotes.
func quoteFlowScalar(v string) string {
	if strings.ContainsAny(v, ",{}[]") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return quoteScalar(v)
}

// plainSafe reports whether v can be written without quotes and read back as itself.
func plainSafe(v string) bool {
	if v == "" || v != strings.TrimSpace(v) {
		return false
	}
	switch v[0] {
	case '"', '\'', '>', '|', '[', ']', '{', '}', '#', '&', '*', '!', '%', '@', '`', '-', '?', ':', ',':
		return false
	}
	return !strings.Contains(v, ": ") && !strings.Contains(v, " #") && !strings.HasSuffix(v, ":") &&
		!strings.ContainsAny(v, "\n\r\\")
}

// Set makes key a single-line scalar with value, replacing the field in place when it exists
// (whatever shape it had) and otherwise adding it after the last field. Any later fields with the
// same key are dropped. It refuses a value that
// would not read back as written, so a successful Set is a promise about the file.
func (d *Document) Set(key, value string) error {
	if !d.HasFrontmatter {
		return fmt.Errorf("ticketstore: %s has no frontmatter to set %q in", d.Path, key)
	}
	if !keyLine.MatchString(key + ":") {
		return fmt.Errorf("ticketstore: %q is not a valid frontmatter key", key)
	}
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("ticketstore: %s: a value for %q must be a single line", d.Path, key)
	}
	if mapKeys[key] {
		return fmt.Errorf("ticketstore: %s: %q is a map and is not set as a scalar", d.Path, key)
	}
	line := renderField(Field{Key: key, Kind: KindScalar, Text: value})
	probe := Parse(d.Path, "---\n"+strings.Join(line, "\n")+"\n---\n")
	got, ok := probe.Get(key)
	if !ok || got.Kind != KindScalar || got.Text != value {
		return fmt.Errorf("ticketstore: %s: %q cannot be written to %q so that it reads back as written", d.Path, value, key)
	}
	nf := Field{Key: key, Kind: KindScalar, Style: got.Style, Text: value}
	first := -1
	kept := d.Fields[:0:0]
	for i, f := range d.Fields {
		if f.Kind != KindFiller && f.Key == key {
			if first >= 0 {
				d.dirty = true
				continue
			}
			first = i
			if f.Kind == KindScalar && canonicalScalar(f.Style) && f.Text == value {
				kept = append(kept, f) // already says this, in whatever quoting it was written
				continue
			}
			nf.Line = f.Line
			f = nf
			d.dirty = true
		}
		kept = append(kept, f)
	}
	if first >= 0 {
		d.Fields = kept
		return nil
	}
	at := len(d.Fields)
	for at > 0 && d.Fields[at-1].Kind == KindFiller {
		at--
	}
	d.Fields = append(d.Fields[:at], append([]Field{nf}, d.Fields[at:]...)...)
	d.dirty = true
	return nil
}

// SetMapEntry makes key: value an entry of the flow map at mapKey (`trackers: {pr: https://...}`),
// replacing the entry when it exists and keeping every other entry. The map is created when absent.
// A map written as an indented block is refused rather than rewritten: other writers of the same
// field read only the flow form, so converting it would strand the block's lines. Like Set, it
// refuses a value that would not read back as written.
func (d *Document) SetMapEntry(mapKey, key, value string) error {
	if !d.HasFrontmatter {
		return fmt.Errorf("ticketstore: %s has no frontmatter to set %q in", d.Path, mapKey)
	}
	if !keyLine.MatchString(mapKey+":") || !keyLine.MatchString(key+":") {
		return fmt.Errorf("ticketstore: %q.%q is not a valid frontmatter key path", mapKey, key)
	}
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("ticketstore: %s: a value for %q.%q must be a single line", d.Path, mapKey, key)
	}
	nf := Field{Key: mapKey, Kind: KindMap, Style: StyleFlow}
	at := -1
	for i, f := range d.Fields {
		if f.Kind == KindFiller || f.Key != mapKey {
			continue
		}
		if at >= 0 {
			return fmt.Errorf("ticketstore: %s: %q appears more than once", d.Path, mapKey)
		}
		if f.Kind != KindMap || f.Style != StyleFlow {
			return fmt.Errorf("ticketstore: %s: %q is %s; only an empty or one-line {k: v} map can have an entry set", d.Path, mapKey, describe(f))
		}
		at, nf.Line = i, f.Line
		nf.Children = append([]Field(nil), f.Children...)
	}
	replaced := false
	for i, c := range nf.Children {
		if c.Key == key {
			nf.Children[i].Text, replaced = value, true
		}
	}
	if !replaced {
		nf.Children = append(nf.Children, Field{Key: key, Kind: KindScalar, Style: StylePlain, Text: value})
	}
	probe := Parse(d.Path, "---\n"+strings.Join(renderField(nf), "\n")+"\n---\n")
	// The probe has none of the required fields, so only a finding about the line itself counts.
	malformed := false
	for _, f := range probe.Findings {
		if f.Code == CodeBadLine || f.Code == CodeBadValue || f.Code == CodeNonCanonical || f.Code == CodeDuplicateKey {
			malformed = true
		}
	}
	if got, ok := probe.Lookup(mapKey, key); !ok || got != value || malformed {
		return fmt.Errorf("ticketstore: %s: %q cannot be written to %q.%q so that it reads back as written", d.Path, value, mapKey, key)
	}
	d.dirty = true
	if at >= 0 {
		d.Fields[at] = nf
		return nil
	}
	end := len(d.Fields)
	for end > 0 && d.Fields[end-1].Kind == KindFiller {
		end--
	}
	d.Fields = append(d.Fields[:end], append([]Field{nf}, d.Fields[end:]...)...)
	return nil
}

// AppendBody adds text to the end of the body, starting it on a fresh line.
func (d *Document) AppendBody(text string) {
	if d.body != "" && !strings.HasSuffix(d.body, "\n") {
		d.body += d.eol
	}
	d.body += text
	d.bodyNL = true
	d.dirty = true
}
