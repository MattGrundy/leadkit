package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A lane's pending/ and in-progress/ hold tickets, and the verbs that watch them (`queue wait`, the
// sweep, the waker) treat every entry there as work. Something that is not a ticket, such as an empty
// directory a mis-targeted `queue move` left behind, therefore wakes an owner for nothing, again and
// again, and no verb can see it or clear it. A ticket entry is a regular file, or a symlink that resolves
// to one, named *.md. Everything else is a STRAY: not counted as work, reported by the sweep, and
// cleared by `queue repair --apply` only when clearing it can lose nothing.

// workStates are the states whose entries are read as work.
var workStates = []string{"pending", "in-progress"}

// isWorkEntry reports whether path is something an owner could claim. A symlink is judged by what it
// resolves to; one that dangles is not a ticket (and is the repair reaper's business, not a stray).
func isWorkEntry(path string) bool {
	if !strings.HasSuffix(path, ".md") {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// listTickets returns the non-dotfile entries directly under dir that are tickets.
func listTickets(dir string) []string {
	var out []string
	for _, name := range listEntries(dir) {
		if isWorkEntry(filepath.Join(dir, name)) {
			out = append(out, name)
		}
	}
	return out
}

// StrayEntry is one entry in a lane's pending/ or in-progress/ that is not a ticket.
type StrayEntry struct {
	Lane      string `json:"lane"`
	State     string `json:"state"`
	Name      string `json:"name"`
	Path      string `json:"-"`
	Kind      string `json:"kind"`      // "directory" | "file" | "symlink"
	Removable bool   `json:"removable"` // clearing it loses nothing: an empty directory tree or an empty file
}

// Rel is the queue-relative path of the entry.
func (s StrayEntry) Rel() string { return s.Lane + "/" + s.State + "/" + s.Name }

// strayEntries lists the strays in one lane. A symlinked lane or state dir is an alias inbox and is left
// alone, as the sweep leaves it.
func strayEntries(root, lane string) []StrayEntry {
	var out []StrayEntry
	for _, state := range workStates {
		dir := filepath.Join(root, lane, state)
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() { // Lstat: a symlinked state dir is an alias inbox, not a dir
			continue
		}
		for _, name := range listEntries(dir) {
			p := filepath.Join(dir, name)
			if isWorkEntry(p) {
				continue
			}
			fi, err := os.Lstat(p)
			if err != nil {
				continue
			}
			s := StrayEntry{Lane: lane, State: state, Name: name, Path: p}
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				if _, serr := os.Stat(p); serr != nil {
					continue // dangling: reported as BROKEN and reaped by repair, not a stray
				}
				s.Kind = "symlink"
			case fi.IsDir():
				s.Kind, s.Removable = "directory", recursivelyEmpty(p)
			default:
				s.Kind, s.Removable = "file", fi.Mode().IsRegular() && fi.Size() == 0
			}
			out = append(out, s)
		}
	}
	return out
}

// FindStrays lists every stray entry in every lane under root.
func FindStrays(root string) ([]StrayEntry, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("no queue dir at %s: %w", root, err)
	}
	var out []StrayEntry
	for _, e := range ents {
		if e.Name() == "ROLES" || !e.IsDir() {
			continue
		}
		out = append(out, strayEntries(root, e.Name())...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel() < out[j].Rel() })
	return out, nil
}

// describe renders what a stray is and what happens to it.
func (s StrayEntry) describe() string {
	switch {
	case s.Removable && s.Kind == "directory":
		return "empty directory"
	case s.Removable:
		return "empty file"
	case s.Kind == "directory":
		return "directory holding files, inspect by hand"
	case s.Kind == "symlink":
		return "symlink to something that is not a ticket, inspect by hand"
	default:
		return "file that is not a ticket, inspect by hand"
	}
}

// removeStray removes a removable stray, re-checking at the moment of removal. os.Remove fails on a
// non-empty directory, so a file that raced in is never deleted.
func removeStray(s StrayEntry) error {
	if !s.Removable {
		return fmt.Errorf("%s is not removable", s.Rel())
	}
	if s.Kind == "directory" {
		return removeEmptyTree(s.Path)
	}
	if fi, err := os.Lstat(s.Path); err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		return fmt.Errorf("%s is no longer an empty file", s.Rel())
	}
	return os.Remove(s.Path)
}
