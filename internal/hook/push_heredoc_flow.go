package hook

import (
	"path"
	"strings"
)

// A heredoc body that mentions `push` is dropped from the reading only if nothing can run it.
// "Nothing" is not "no wrapper anywhere on the command line": a wrapper (`python3`, `xargs`,
// `awk`, `env`, `find`) that the body cannot reach is not a reason to refuse it
// (`git commit -F - <<EOF … push … EOF` and, two lines later, `gh pr list | python3 -c …`).
//
// wrapperMayRunBody asks whether a wrapper can be reached from the body, and says "may" whenever
// it cannot show it does not. It is a proof of safety over a command it can fully read, not a
// parser of the shell:
//
//   - the command line must be plain: simple commands joined by `|`, `|&`, `;`, `&&`, `||`,
//     `&` and newlines. A compound command, a function, a group, a process substitution or
//     any command substitution that is not the heredoc's own carrier means data can reach a
//     shell by a route this reading does not follow, so the answer is "may";
//   - the body's carrier is found, and its stdout goes nowhere but the next pipe stage (not
//     a file, not `tee`, which a later `sh file` could run);
//   - every stage of the carrier's pipeline, before and after it, is a command known to pass
//     its input on or consume it, and none is a wrapper or a command word the hook cannot read.
//
// A wrapper in another pipeline (after `;`, `&&`, `||`, `&` or a newline) is then not
// connected to the body. Anything that is not shown to be so is "may".

// compoundWords open or close a compound command or a group; a command line holding one is
// not read as a flat list of pipelines (a `done` ends a loop whose body a pipe fed).
var compoundWords = map[string]bool{
	"{": true, "}": true, "(": true, ")": true, "[[": true, "]]": true,
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"while": true, "until": true, "do": true, "done": true, "for": true, "select": true,
	"case": true, "esac": true, "in": true, "function": true, "coproc": true,
}

// redirectsStdout reports whether a redirect operator (as the lexer spells it, with any fd
// prefix) sends the command's stdout to a file or descriptor: `>`, `>>`, `>|`, `&>`, `1>`,
// `>&2`. `2>`, `2>&1` and the input redirects do not.
func redirectsStdout(op string) bool {
	if strings.HasPrefix(op, "&>") {
		return true
	}
	op = strings.TrimPrefix(op, "1")
	return strings.HasPrefix(op, ">")
}

// isHeredocOperator reports whether a redirect operator opens a heredoc: `<<` or `<<-`
// (with any fd prefix), not the here-string `<<<`.
func isHeredocOperator(op string) bool {
	op = strings.TrimLeft(op, "0123456789")
	return strings.HasPrefix(op, "<<") && !strings.HasPrefix(op, "<<<")
}

// linkIsPipe reports whether the separators between two segments join them in one pipeline.
// `||` does not; anything else holding a `|` is read as a pipe, which can only widen a
// pipeline and so only keep a refusal.
func linkIsPipe(link string) bool {
	return strings.Contains(link, "|") && !strings.Contains(link, "||")
}

// stageKeepsNothing reports whether a pipeline stage is a command that passes its input on or
// consumes it (the heredocConsumers, bar `tee`), with nothing that writes it to a file: no
// stdout redirect, no `sort -o`. A stage outside that list (`jq`, `sed`, `dd of=`, `sponge`, a
// wrapper) may keep the text or run it, and a wrapper elsewhere on the line may then run what
// it kept; this reading does not follow that, so it does not call the stage safe. It is a list of what
// is known to be inert, not of what is known to be dangerous.
func stageKeepsNothing(sg segment) bool {
	ci := commandStart(sg.words)
	if ci >= len(sg.words) || sg.dynamic[ci] || sg.outRedirect {
		return false
	}
	name := path.Base(sg.words[ci])
	if name == "tee" || !heredocConsumers[name] {
		return false
	}
	if name == "sort" {
		for _, w := range sg.words[ci+1:] {
			if strings.HasPrefix(w, "-o") || strings.HasPrefix(w, "--output") || (strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "o")) {
				return false
			}
		}
	}
	return true
}

// substitutionOpeners are the spellings of a nested command the segment reading cannot see
// into: the lexer hands one over as a part of a word.
var substitutionOpeners = []string{"$(", "`", "<(", ">("}

func hasSubstitution(w string) bool {
	for _, o := range substitutionOpeners {
		if strings.Contains(w, o) {
			return true
		}
	}
	return false
}

// carriesHeredoc reports whether a word is a command substitution that holds a heredoc
// (`"$(cat <<'EOF' … EOF)"`): the idiom for a commit or PR message.
func carriesHeredoc(w string) bool {
	return hasSubstitution(w) && strings.Contains(w, "<<")
}

// wordNamesWrapper reports whether a word holds a wrapper command as a word of its own
// (`$(cat <<EOF | sh`). The heredoc body is already stripped from it, so what is left is
// the command skeleton plus whatever else the quoted argument says.
func wordNamesWrapper(w string) bool {
	for _, tok := range strings.FieldsFunc(w, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == '/')
	}) {
		if shellWrappers[path.Base(tok)] {
			return true
		}
	}
	return false
}

// flatCommandLine reports whether segs are plain pipelines: only the separators named above,
// no compound command, and no command substitution other than a heredoc's carrier, which
// must be an argument (`git commit -m "$(cat <<EOF …)"`), never an assignment or the
// command word, whose value the reading would lose track of.
func flatCommandLine(segs []segment) bool {
	for _, sg := range segs {
		if strings.Trim(sg.link, "|&;\n\r \t") != "" {
			return false // a `(`, `)`, `{`, `}` or other separator the reading does not follow
		}
		ci := commandStart(sg.words)
		for j, w := range sg.words {
			if compoundWords[w] {
				return false
			}
			if hasSubstitution(w) && !(carriesHeredoc(w) && j > ci) {
				return false
			}
		}
	}
	return true
}

// heredocOperators counts the heredocs a segment opens: `<<` redirects (which the segment
// reading drops, so they are counted by the lexer) plus those inside a command substitution
// carried in a word. A here-string (`<<<`) is not a heredoc.
func heredocOperators(sg segment) int {
	n := sg.hereOps
	for _, w := range sg.words {
		if carriesHeredoc(w) {
			n += strings.Count(strings.ReplaceAll(w, "<<<", ""), "<<")
		}
	}
	return n
}

// wrapperMayRunBody is the question the heredoc rule asks of a command that holds a wrapper
// (see the comment above): can a wrapper in segs run the body of any of its nbodies
// heredocs? It answers true unless it can show otherwise. The heredocs are found by the
// segment that opens them, the carrier; if the reading cannot account for every body, it
// has lost one and says true.
func wrapperMayRunBody(segs []segment, nbodies int) bool {
	if !flatCommandLine(segs) {
		return true
	}
	opened := 0
	for k, sg := range segs {
		ops := heredocOperators(sg)
		if ops == 0 {
			continue
		}
		opened += ops
		ci := commandStart(sg.words)
		if ci >= len(sg.words) {
			// `<<EOF` with no command word (`< file`, or only redirects): nothing to name.
			return true
		}
		if sg.dynamic[ci] {
			return true // not a command the hook can name
		}
		for _, w := range sg.words[ci+1:] {
			if carriesHeredoc(w) && wordNamesWrapper(w) {
				return true // `$(cat <<EOF | sh`: the substitution itself pipes into a wrapper
			}
		}
		lo, hi := k, k
		for lo > 0 && linkIsPipe(segs[lo].link) {
			lo--
		}
		for hi+1 < len(segs) && linkIsPipe(segs[hi+1].link) {
			hi++
		}
		for _, m := range segs[lo : hi+1] {
			if !stageKeepsNothing(m) {
				return true
			}
		}
	}
	return opened < nbodies
}
