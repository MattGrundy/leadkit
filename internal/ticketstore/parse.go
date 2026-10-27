package ticketstore

import (
	"fmt"
	"regexp"
	"strings"
)

// srcLine is one frontmatter line with its 1-based line number in the file and no line ending.
type srcLine struct {
	n int
	s string
}

// keyLine matches `key: value` (or a bare `key:`), after indentation is removed. It is the key
// grammar: a key is word characters and `-`, and the colon must be followed by whitespace or the
// end of the line, so `key:value` is not a key line.
var keyLine = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*):(?:[ \t]+(.*))?$`)

// Parse reads a ticket file's text. It never fails: whatever it cannot read becomes a Finding on
// the returned Document, with HasFrontmatter false when there is no frontmatter block to read.
func Parse(path, text string) *Document {
	d := &Document{Path: path, src: text, eol: "\n"}
	lines := strings.Split(text, "\n")
	if strings.TrimRight(lines[0], " \t\r") != "---" {
		d.add(0, CodeNoFrontmatter, "no opening `---` frontmatter delimiter")
		return d
	}
	if strings.HasSuffix(lines[0], "\r") {
		d.eol = "\r\n"
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r") == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		d.add(0, CodeUnclosed, "the frontmatter block never closes")
		return d
	}
	d.HasFrontmatter = true
	d.open, d.close = "---", "---"
	d.bodyNL = end < len(lines)-1
	d.body = strings.Join(lines[end+1:], "\n")

	fm := make([]srcLine, 0, end-1)
	for i := 1; i < end; i++ {
		fm = append(fm, srcLine{n: i + 1, s: strings.TrimRight(lines[i], "\r")})
	}
	d.Fields = d.parseTop(fm)
	d.checkDuplicateKeys()
	d.validate()
	return d
}

func (d *Document) add(line int, code, msg string) {
	d.Findings = append(d.Findings, Finding{Path: d.Path, Line: line, Code: code, Msg: msg})
}

func indentOf(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	return n
}

func isBlankOrComment(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.HasPrefix(t, "#")
}

func isDash(s string) bool {
	t := strings.TrimSpace(s)
	return t == "-" || strings.HasPrefix(t, "- ")
}

// openField is a top-level field whose continuation lines are still being collected.
type openField struct {
	key, val string
	line     srcLine
	cont     []srcLine
	raw      []string
}

// parseTop splits the frontmatter into top-level fields. A field is its key line and every line
// that follows it indented, or a column-0 `- ` list item, which YAML allows under a key. Blank and
// comment lines between fields become KindFiller entries so a write can reproduce them.
func (d *Document) parseTop(fm []srcLine) []Field {
	var out []Field
	var pending []srcLine // blank/comment lines not yet attributed
	flushFiller := func() {
		for _, p := range pending {
			out = append(out, Field{Kind: KindFiller, Line: p.n, raw: []string{p.s}})
		}
		pending = nil
	}
	var cur *openField
	finish := func() {
		if cur == nil {
			return
		}
		f := d.decodeField(cur.key, cur.val, cur.line.n, cur.cont, 0)
		f.raw = cur.raw
		out = append(out, f)
		cur = nil
	}
	for _, l := range fm {
		switch {
		case isBlankOrComment(l.s):
			pending = append(pending, l)
		case indentOf(l.s) > 0 || (cur != nil && isDash(l.s)):
			if cur == nil {
				flushFiller()
				d.add(l.n, CodeBadLine, fmt.Sprintf("%q is indented but belongs to no key", strings.TrimSpace(l.s)))
				out = append(out, Field{Kind: KindFiller, Line: l.n, raw: []string{l.s}})
				continue
			}
			for _, p := range pending {
				cur.raw = append(cur.raw, p.s)
				cur.cont = append(cur.cont, p)
			}
			pending = nil
			cur.raw = append(cur.raw, l.s)
			cur.cont = append(cur.cont, l)
		default:
			finish()
			flushFiller()
			m := keyLine.FindStringSubmatch(l.s)
			if m == nil {
				d.add(l.n, CodeBadLine, fmt.Sprintf("%q is not a `key: value` line", strings.TrimSpace(l.s)))
				out = append(out, Field{Kind: KindFiller, Line: l.n, raw: []string{l.s}})
				continue
			}
			cur = &openField{key: m[1], val: strings.TrimSpace(m[2]), line: l, raw: []string{l.s}}
		}
	}
	finish()
	flushFiller()
	return out
}

// decodeField turns a key, the rest of its line and its continuation lines into a Field. indent is
// the indentation of the key line, so nested continuation can be measured against it.
func (d *Document) decodeField(key, val string, line int, cont []srcLine, indent int) Field {
	f := Field{Key: key, Line: line}
	body := trimBlank(cont)
	if strings.HasPrefix(val, "#") {
		val = "" // `key:  # comment`: no value on this line
	}

	switch {
	case val == "" && len(body) == 0:
		f.Kind, f.Style = KindScalar, StylePlain
	case val == "":
		return d.decodeBlock(f, body, indent)
	case val[0] == '>' || val[0] == '|':
		f.Kind = KindScalar
		f.Style = StyleFolded
		if val[0] == '|' {
			f.Style = StyleLiteral
		}
		f.Text = blockScalar(body, val[0] == '>')
		if rest := strings.TrimLeft(val[1:], "+-0123456789"); rest != "" && !strings.HasPrefix(strings.TrimSpace(rest), "#") {
			d.add(line, CodeBadValue, fmt.Sprintf("key %q: %q is not a block scalar header", key, val))
		}
	case val[0] == '[' || val[0] == '{':
		f = d.decodeFlow(f, val)
		if len(body) > 0 {
			d.add(line, CodeBadValue, fmt.Sprintf("key %q: a flow value cannot continue on indented lines", key))
		}
	case val[0] == '"' || val[0] == '\'':
		f.Kind = KindScalar
		f.Style = StyleDouble
		if val[0] == '\'' {
			f.Style = StyleSingle
		}
		text, why := decodeQuoted(val)
		f.Text = text
		if why != "" {
			d.add(line, CodeBadValue, fmt.Sprintf("key %q: %s", key, why))
		}
		if len(body) > 0 {
			d.add(line, CodeBadValue, fmt.Sprintf("key %q: a quoted value cannot continue on indented lines", key))
		}
	default:
		f.Kind, f.Style, f.Text = KindScalar, StylePlain, val
		if len(body) > 0 {
			// A plain scalar continued on indented lines folds to one line.
			f.Style = StyleMultiline
			parts := []string{val}
			for _, c := range body {
				if t := strings.TrimSpace(c.s); t != "" {
					parts = append(parts, t)
				}
			}
			f.Text = strings.Join(parts, " ")
		}
	}
	return f
}

// trimBlank drops leading and trailing blank lines from a continuation.
func trimBlank(cont []srcLine) []srcLine {
	for len(cont) > 0 && strings.TrimSpace(cont[0].s) == "" {
		cont = cont[1:]
	}
	for len(cont) > 0 && strings.TrimSpace(cont[len(cont)-1].s) == "" {
		cont = cont[:len(cont)-1]
	}
	return cont
}

// decodeBlock reads an indented block under a key with no inline value: a list when every content
// line is a `- ` item, otherwise a map.
func (d *Document) decodeBlock(f Field, body []srcLine, indent int) Field {
	f.Style = StyleBlock
	var content []srcLine
	for _, l := range body {
		if !isBlankOrComment(l.s) {
			content = append(content, l)
		}
	}
	allDash := len(content) > 0
	for _, l := range content {
		if !isDash(l.s) {
			allDash = false
		}
	}
	if allDash {
		f.Kind = KindList
		for _, l := range content {
			t := strings.TrimSpace(l.s)
			item := strings.TrimSpace(strings.TrimPrefix(t, "-"))
			text, why := decodeItem(item)
			if why != "" {
				d.add(l.n, CodeBadValue, fmt.Sprintf("key %q: list item %s", f.Key, why))
			}
			f.Items = append(f.Items, text)
		}
		return f
	}
	f.Kind = KindMap
	f.Children = d.parseNested(content, f.Key)
	return f
}

// parseNested reads a nested block map. Its entries sit at the indentation of its first line;
// anything deeper belongs to the entry above it.
func (d *Document) parseNested(content []srcLine, parent string) []Field {
	if len(content) == 0 {
		return nil
	}
	ind := indentOf(content[0].s)
	var out []Field
	var cur *openField
	finish := func() {
		if cur != nil {
			out = append(out, d.decodeField(cur.key, cur.val, cur.line.n, cur.cont, ind))
			cur = nil
		}
	}
	for _, l := range content {
		if indentOf(l.s) > ind && cur != nil {
			cur.cont = append(cur.cont, l)
			continue
		}
		finish()
		m := keyLine.FindStringSubmatch(strings.TrimSpace(l.s))
		if indentOf(l.s) != ind || m == nil {
			d.add(l.n, CodeBadLine, fmt.Sprintf("under %q: %q is not a `key: value` line at the block's indentation", parent, strings.TrimSpace(l.s)))
			continue
		}
		cur = &openField{key: m[1], val: strings.TrimSpace(m[2]), line: l}
	}
	finish()
	return out
}

// blockScalar decodes the content of a `>` (folded) or `|` (literal) scalar: the common
// indentation is removed and, for a folded scalar, adjacent lines join with a space and a blank
// line becomes a newline. Trailing newlines are dropped.
func blockScalar(body []srcLine, folded bool) string {
	min := -1
	for _, l := range body {
		if strings.TrimSpace(l.s) == "" {
			continue
		}
		if i := indentOf(l.s); min == -1 || i < min {
			min = i
		}
	}
	var rows []string
	for _, l := range body {
		if strings.TrimSpace(l.s) == "" {
			rows = append(rows, "")
			continue
		}
		rows = append(rows, strings.TrimRight(l.s[min:], " \t"))
	}
	if !folded {
		return strings.Join(rows, "\n")
	}
	var b strings.Builder
	for i, r := range rows {
		switch {
		case r == "":
			b.WriteString("\n")
		case i > 0 && rows[i-1] != "":
			b.WriteString(" " + r)
		default:
			b.WriteString(r)
		}
	}
	return b.String()
}

// decodeQuoted reads a quoted scalar followed by nothing, or by a ` # comment`. A double-quoted
// value resolves \" and \; a single-quoted one resolves ”. Any other backslash is left as
// written, which is what every other reader of these files does.
func decodeQuoted(v string) (string, string) {
	q := v[0]
	var b strings.Builder
	for i := 1; i < len(v); i++ {
		c := v[i]
		switch {
		case q == '"' && c == '\\' && i+1 < len(v) && (v[i+1] == '"' || v[i+1] == '\\'):
			b.WriteByte(v[i+1])
			i++
		case q == '\'' && c == '\'' && i+1 < len(v) && v[i+1] == '\'':
			b.WriteByte('\'')
			i++
		case c == q:
			after := v[i+1:]
			rest := strings.TrimSpace(after)
			commentOK := strings.HasPrefix(rest, "#") && (after[0] == ' ' || after[0] == '\t')
			if rest != "" && !commentOK {
				return b.String(), fmt.Sprintf("unexpected text %q after the closing quote", rest)
			}
			return b.String(), ""
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimPrefix(v, string(q)), "an unterminated quoted string"
}

// decodeItem reads one list item: a quoted scalar, or anything else as written (a flow map such as
// `{ticket: 102}` is kept as its text).
func decodeItem(item string) (string, string) {
	if item == "" {
		return "", ""
	}
	if item[0] == '"' || item[0] == '\'' {
		return decodeQuoted(item)
	}
	return item, ""
}

// decodeFlow reads a one-line `[a, b]` or `{k: v}`. Nesting is not supported and is reported.
func (d *Document) decodeFlow(f Field, val string) Field {
	f.Style = StyleFlow
	closeCh := byte(']')
	f.Kind = KindList
	if val[0] == '{' {
		closeCh = '}'
		f.Kind = KindMap
	}
	end := strings.LastIndexByte(val, closeCh)
	if end == -1 {
		d.add(f.Line, CodeBadValue, fmt.Sprintf("key %q: unterminated flow value %q", f.Key, val))
		return f
	}
	if rest := strings.TrimSpace(val[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		d.add(f.Line, CodeBadValue, fmt.Sprintf("key %q: unexpected text %q after the flow value", f.Key, rest))
	}
	inner := strings.TrimSpace(val[1:end])
	if inner == "" {
		return f
	}
	for _, part := range splitFlow(inner) {
		part = strings.TrimSpace(part)
		if f.Kind == KindList {
			text, why := decodeItem(part)
			if why != "" {
				d.add(f.Line, CodeBadValue, fmt.Sprintf("key %q: flow item %s", f.Key, why))
			}
			f.Items = append(f.Items, text)
			continue
		}
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			d.add(f.Line, CodeBadValue, fmt.Sprintf("key %q: flow entry %q is not `key: value`", f.Key, part))
			continue
		}
		text, why := decodeItem(strings.TrimSpace(v))
		if why != "" {
			d.add(f.Line, CodeBadValue, fmt.Sprintf("key %q: flow entry %q %s", f.Key, strings.TrimSpace(k), why))
		}
		f.Children = append(f.Children, Field{Key: strings.TrimSpace(k), Kind: KindScalar, Style: StylePlain, Text: text, Line: f.Line})
	}
	return f
}

// splitFlow splits on commas outside quotes.
func splitFlow(s string) []string {
	var parts []string
	var q byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == ',':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func (d *Document) checkDuplicateKeys() {
	seen := map[string]int{}
	for _, f := range d.Fields {
		if f.Kind == KindFiller {
			continue
		}
		if first, dup := seen[f.Key]; dup {
			d.add(f.Line, CodeDuplicateKey, fmt.Sprintf("key %q is repeated (first at line %d); the first one is the one read", f.Key, first))
			continue
		}
		seen[f.Key] = f.Line
	}
}
