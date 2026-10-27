// Package state defines the two uniform state-file schemas leadkit reads from known,
// per-project paths (declared in the profile — see internal/config): the Session
// Goal (the durable north star) and the Handover (durable cross-compaction state).
// Both are markdown files with a leading YAML frontmatter block holding every
// structured field, followed by a human-readable body rendered from that same data.
//
// internal/viewer and internal/review already parse a similar "---\n...\n---\n"
// frontmatter convention, but as flat key:value lines — Goal/Handover need real lists
// and nested records (definition_of_done, in_flight, roster_snapshot), so this
// package decodes the block as YAML directly rather than sharing that flatter parser.
// See docs/PORTABLE-OPERATING-KIT.md.
package state

import (
	"fmt"
	"strings"
)

// splitFrontmatter separates a leading "---\n<yaml>\n---\n" block from the rest of a
// file's content, mirroring the delimiter convention internal/viewer and
// internal/review already use.
func splitFrontmatter(data []byte) (yamlBlock []byte, body string, err error) {
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", fmt.Errorf("no opening frontmatter delimiter")
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx == -1 {
		return nil, "", fmt.Errorf("frontmatter block never closes")
	}
	yamlBlock = []byte(rest[:idx])
	body = strings.TrimPrefix(rest[idx+len("\n---"):], "\n")
	return yamlBlock, body, nil
}
