// Package starter carries the adoption assets inside the binary, so adopting leadkit needs
// no repo checkout — the same reason internal/onboard embeds its guide and skill.
//
// The assets are STARTERS, deliberately: written to be replaced, and each carries a marker so
// a leadkit-generated file can be told from one a human has edited — the convention
// internal/team's starter SOP uses.
//
// This package does not decide which definition a session uses, and nothing in leadkit does.
// Claude Code reads agent definitions itself and never sees the embedded set; the starters only
// take effect once `leadkit team install` has written them into the user's agents directory,
// and that install is the whole of leadkit's "resolution": an existing file is kept unless the
// caller asks to replace it. Do not add a precedence API here without a caller that applies it.
package starter

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed profile.leadkit.yaml
var profileTemplate []byte

//go:embed team-protocol.md
var teamProtocol []byte

// The starter agent set: the roles the operating model itself needs, plus a small set of
// generic specialists, written clean rather than captured from any
// project — a definition embedded in every binary travels further and more silently than a
// repo file, so none of these may name a real project, person or product.
//
// There is deliberately NO polling agent. Watching a long-running target is a blocking
// command's job, not an agent's: a command costs nothing while it waits, where an agent
// spends tokens per tick and improvises when it hits something it cannot read.
//
//go:embed agents
var agentsFS embed.FS

// ProtocolMarker is the first line of the generated starter protocol. Exported so a
// caller can recognise it without duplicating the literal.
const ProtocolMarker = "<!-- leadkit: GENERATED STARTER PROTOCOL — replace with your team's own -->"

// ProfilePlaceholder is the value in the profile template that an adopter is expected to
// replace.
const ProfilePlaceholder = "<your-project>"

// ProfileTemplate returns the starter leadkit.yaml. The copy is defensive: a caller
// mutating the returned slice must not corrupt the embedded asset for the rest of the
// process.
func ProfileTemplate() []byte { return append([]byte(nil), profileTemplate...) }

// TeamProtocol returns the starter team protocol.
func TeamProtocol() []byte { return append([]byte(nil), teamProtocol...) }

// AgentMarker is the line every generated starter agent definition carries, immediately
// after its YAML frontmatter (the frontmatter must come first for the harness to parse it,
// so the marker cannot be the literal first line as it is for the SOP and protocol).
const AgentMarker = "<!-- leadkit: GENERATED STARTER AGENT — replace with your team's own -->"

// Agents returns the starter agent definitions, keyed by file name.
func Agents() (map[string][]byte, error) {
	entries, err := agentsFS.ReadDir("agents")
	if err != nil {
		return nil, fmt.Errorf("read embedded agents: %w", err)
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		// Same filter readAgentDir applies. Without it a non-.md file here would enter the
		// embedded set, never match a local definition, and report as permanently removed.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := agentsFS.ReadFile("agents/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read embedded agent %s: %w", e.Name(), err)
		}
		out[e.Name()] = data
	}
	return out, nil
}

// IsGeneratedAgent reports whether data is still a leadkit-generated starter definition.
// The marker sits after the frontmatter, so this looks anywhere in the leading portion
// rather than only at the start.
func IsGeneratedAgent(data []byte) bool {
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	return bytes.Contains(head, []byte(AgentMarker))
}

// IsGeneratedProtocol reports whether data is still a leadkit-generated starter, i.e. its
// first non-blank content is ProtocolMarker. Mirrors internal/team's isGeneratedStarter so
// the two cannot disagree about what "generated" means.
func IsGeneratedProtocol(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte(ProtocolMarker))
}

// ErrExists reports that the destination was left untouched because something is already
// there. A sentinel rather than a bare error so a caller can treat "already adopted" as the
// ordinary case it is, not a failure.
var ErrExists = fmt.Errorf("already exists")

// Write writes data to path, CREATE-ONLY: an existing file is never overwritten, and
// ErrExists is returned instead. Adoption must be safe to re-run — someone will run
// `init` twice — and silently replacing a profile a team has tuned would be the worst
// possible behaviour for a convenience command.
//
// The write is atomic (temp file in the same directory, then rename) so an interrupted run
// cannot leave a half-written profile that parses as valid-but-wrong.
func Write(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s: %w", path, ErrExists)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".leadkit-starter-*")
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Sync before the rename, or the durability claim above is only true for a killed
	// process and not for a machine that loses power: the rename can land while the
	// contents are still in the page cache, surfacing a zero-length profile.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	// Re-check immediately before the rename. This narrows, and cannot close, the window
	// where a concurrent run created the file after the stat above — rename would clobber
	// it. Two `init` runs racing is unlikely but the cost of losing a tuned profile is
	// high enough to be worth the second look.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s: %w", path, ErrExists)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	return nil
}
