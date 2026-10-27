package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readBack re-reads the ticket after an append. A variable only so a test can make the read
// disagree with the write and prove the check fires.
var readBack = os.ReadFile

// noteTarget decides which file a note goes to, and refuses rather than guess.
//
//   - A lane entry that is a SYMLINK and still resolves names its ticket: the note goes to the
//     real file it points at, wherever that store is. Keeping only the base name and joining it
//     onto this session's tasks dir would put a note meant for another project's ticket into
//     this one's whenever ticket numbers collide, and report success.
//   - A regular file under this session's tasks dir is that ticket.
//   - Anything else (the entry was routed away and does not exist, or a stray regular file
//     occupies the lane path) falls back to tasks/<base> by NAME, which is the case this verb
//     exists for. If no such ticket exists, nothing is written.
//   - But a PATH that names neither this session's queue nor its tasks dir cannot be taken to mean
//     this session's same-named ticket: ticket numbers collide across projects, and a routed-away
//     path from another project's queue would land the note in the wrong ticket and report success.
//     It is refused. A bare name carries no directory and still falls back.
func (rv *Reviewer) noteTarget(task string) (string, error) {
	if strings.ContainsRune(task, filepath.Separator) {
		if fi, err := os.Lstat(task); err == nil {
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				if real, err := filepath.EvalSymlinks(task); err == nil {
					if rfi, err := os.Stat(real); err == nil && rfi.Mode().IsRegular() {
						return real, nil
					}
				}
			case fi.Mode().IsRegular() && underDir(task, rv.TasksDir):
				abs, err := filepath.Abs(task)
				if err == nil {
					return abs, nil
				}
			}
		}
	}
	if strings.ContainsRune(task, filepath.Separator) && !underDirOrMissing(task, rv.QueueRoot) && !underDirOrMissing(task, rv.TasksDir) {
		return "", fmt.Errorf("task note: refuse — %s is outside this session's queue (%s) and tasks (%s) dirs and no longer resolves, so it cannot be said which ticket it meant; pass the ticket's tasks/ path or run from its project (nothing was written anywhere)", task, rv.QueueRoot, rv.TasksDir)
	}
	base := filepath.Base(task)
	if !strings.HasSuffix(base, ".md") {
		base += ".md"
	}
	canonical := filepath.Join(rv.TasksDir, base)
	if fi, err := os.Stat(canonical); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("task note: refuse — no ticket %s under the tasks dir %s (nothing was written anywhere)", base, rv.TasksDir)
	}
	return canonical, nil
}

// underDir reports whether path lies inside dir, comparing symlink-resolved spellings.
func underDir(path, dir string) bool {
	p, err1 := filepath.EvalSymlinks(path)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// underDirOrMissing is underDir for a path that may not exist (a lane entry that was routed
// away): the deepest ancestor that does exist is symlink-resolved and the rest is appended
// lexically, so a /var and /private/var spelling of one queue still compare equal.
func underDirOrMissing(path, dir string) bool {
	p, err1 := resolveExistingPrefix(path)
	d, err2 := resolveExistingPrefix(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveExistingPrefix(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no part of %s exists", path)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// NoteResult says where a note landed.
type NoteResult struct {
	Path  string // the canonical tasks/<base> file the note was appended to
	Bytes int
}

// AppendNote appends a review note to a ticket's CANONICAL file, tasks/<base>, and fails loudly
// if it cannot.
//
// A ticket is reached through its lane path (queue/<lane>/<state>/<base>), a symlink into
// tasks/ that another command moves when the ticket is routed. A shell `>>` to a lane path that
// has been routed away does not fail: it creates a fresh regular file at the lane path, exits 0,
// and the note is in neither the ticket nor anything that reads it. So a lane path that still
// resolves names the real ticket file, and one that does not falls back to tasks/<base> by
// NAME (see noteTarget); the append happens under the same lock a frontmatter rewrite takes, and
// the result is read back before reporting success. The path reported is the file written.
func (rv *Reviewer) AppendNote(task, by, text string, now time.Time) (NoteResult, error) {
	by, text = strings.TrimSpace(by), strings.TrimRight(text, " \t\r\n")
	if by == "" {
		return NoteResult{}, fmt.Errorf("task note: refuse — --by is required, a note with no author is not a review record")
	}
	if strings.TrimSpace(text) == "" {
		return NoteResult{}, fmt.Errorf("task note: refuse — the note is empty")
	}
	canonical, err := rv.noteTarget(task)
	if err != nil {
		return NoteResult{}, err
	}

	entry := fmt.Sprintf("\n## %s — %s\n\n%s\n", by, now.UTC().Format("2006-01-02T15:04:05Z"), text)
	err = withTicketLock(canonical, func() error {
		f, err := os.OpenFile(canonical, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("task note: open %s: %w", canonical, err)
		}
		if _, err := f.WriteString(entry); err != nil {
			f.Close()
			return fmt.Errorf("task note: write %s: %w", canonical, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("task note: close %s: %w", canonical, err)
		}
		// Contains, not HasSuffix: a writer that ignores the lock may append right after ours, and
		// the note has still landed.
		data, err := readBack(canonical)
		if err != nil || !strings.Contains(string(data), entry) {
			return fmt.Errorf("task note: wrote to %s but could not read the note back; do not assume it landed", canonical)
		}
		return nil
	})
	if err != nil {
		return NoteResult{}, err
	}
	return NoteResult{Path: canonical, Bytes: len(entry)}, nil
}
