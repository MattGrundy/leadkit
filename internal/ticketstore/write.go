package ticketstore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// maxAttempts bounds the compare-and-swap loop.
const maxAttempts = 64

// ErrConflict is returned when the file kept changing under every attempt.
var ErrConflict = errors.New("ticketstore: the ticket changed under every attempt to write it")

// beforeCommit runs after the edit has been computed and before the commit checks the file again.
// It exists so a test can be the concurrent writer at the one moment that matters.
var beforeCommit = func(path string) {}

// Edit changes a Document. It is called again on a fresh read after a lost race, so it must derive
// everything from the Document it is given and touch nothing else.
type Edit func(d *Document) error

// Update is the one way a ticket file is written: read it, apply edit, validate the result, and
// swap it in only if the file is still the bytes that were read.
//
// The write is a compare-and-swap on the file's content hash:
//
//  1. read the file and hash it;
//  2. apply the edit and refuse the result if it has any Finding;
//  3. write the new text to a temp file beside it;
//  4. under the ticket's advisory lock, hash the file again; if it differs, discard the temp file
//     and start over from step 1; if not, rename the temp file over it.
//
// A path that is a symlink (a lane entry) is resolved first, so the swap replaces the ticket, never
// the link. The returned Document is what was written, or the unchanged file when the edit changed
// nothing (no write, and no new mtime). A file with any Finding after the edit is refused, so a
// Finding already in the file blocks the write until the edit removes it.
func Update(path string, edit Edit) (*Document, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("ticketstore: %w", err)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Jitter, so writers that collided do not collide again in step.
			time.Sleep(time.Duration(rand.IntN(min(attempt, 8)+1)) * time.Millisecond)
		}
		data, err := os.ReadFile(canonical)
		if err != nil {
			return nil, fmt.Errorf("ticketstore: %w", err)
		}
		doc := Parse(canonical, string(data))
		if !doc.HasFrontmatter {
			return nil, fmt.Errorf("ticketstore: refusing to write %s: %s", canonical, summarize(doc.Findings))
		}
		if err := edit(doc); err != nil {
			return nil, err
		}
		out := doc.Render()
		if after := Parse(canonical, out); len(after.Findings) > 0 {
			return nil, fmt.Errorf("ticketstore: refusing to write %s: %s", canonical, summarize(after.Findings))
		}
		if out == string(data) {
			return doc, nil
		}
		done, err := commit(canonical, sha256.Sum256(data), []byte(out))
		if err != nil {
			return nil, err
		}
		if done {
			return Parse(canonical, out), nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrConflict, canonical)
}

// commit swaps out in for the file at path if the file still hashes to want. It reports false, and
// writes nothing, when it does not.
func commit(path string, want [sha256.Size]byte, out []byte) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("ticketstore: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("ticketstore: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return false, fmt.Errorf("ticketstore: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return false, fmt.Errorf("ticketstore: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("ticketstore: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("ticketstore: %w", err)
	}

	beforeCommit(path)

	swapped := false
	err = withFileLock(path, func() error {
		now, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if sha256.Sum256(now) != want {
			return nil
		}
		if err := os.Rename(tmpName, path); err != nil {
			return err
		}
		swapped, keep = true, true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("ticketstore: %w", err)
	}
	return swapped, nil
}
