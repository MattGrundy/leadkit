// Package roles resolves a canonical ROLE (e.g. "code-review") to the LIVE queue
// directory name currently serving it (e.g. "cr-1"), via the queue/ROLES routing
// file — the shared helper send-to-review.sh and gate-verdict.sh both call as
// role(name), so routing always goes through one file instead of a hardcoded name
// that goes stale the moment an agent respawns under a suffixed name.
package roles

import (
	"bufio"
	"os"
	"strings"
)

// Resolve returns the live name mapped to role in rolesPath: the second
// whitespace-separated token of the FIRST line whose first token equals role. Blank
// lines and lines starting with "#" are ignored. Mirrors the awk `$1==r{print $2}`
// the bash scripts use. ok is false if rolesPath can't be read or role has no mapping.
func Resolve(rolesPath, role string) (live string, ok bool) {
	f, err := os.Open(rolesPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == role {
			return fields[1], true
		}
	}
	return "", false
}

// Entry is one role -> live-name row, as List returns it.
type Entry struct {
	Role string
	Live string
}

// List returns every role -> live mapping in rolesPath, in file order — blank/comment
// lines skipped, and (unlike Resolve) only a line with EXACTLY two whitespace fields
// counts as a row; assign.sh's "known roles" listing (`awk '!/^#/ && NF==2 {...}'`)
// uses the same rule, so this mirrors it for that error message. A missing/unreadable
// rolesPath returns nil, not an error — callers already know how to report that.
func List(rolesPath string) []Entry {
	f, err := os.Open(rolesPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) == 2 {
			out = append(out, Entry{Role: fields[0], Live: fields[1]})
		}
	}
	return out
}
