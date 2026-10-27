package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UnlinkResult is what Unlink removed, or would remove.
type UnlinkResult struct {
	Link      string   // the overlay symlink
	StoreFile string   // what it resolves to ("" when dangling)
	Remaining []string // queue-relative paths of the OTHER links to the same store file
	DryRun    bool
	Removed   bool
}

// Unlink removes ONE overlay link: the governed alternative to a raw rm, which queue-guard
// refuses, for a stray or duplicate link. It is deliberately narrower than RemoveTask, which
// deletes every link and the store file.
//
// It refuses anything that is not a symlink (a plain file or directory is never a link to
// remove), anything not shaped <lane>/<dir>/<ticket> inside the queue root, and, unless
// allowLast, a link that is the ticket's ONLY one, because removing that leaves the ticket in no
// lane at all. A dangling link has no store file to compare and is removable (queue repair
// reaps those too).
func Unlink(root, link string, allowLast, apply bool) (UnlinkResult, error) {
	res := UnlinkResult{DryRun: !apply}
	if _, err := os.Lstat(link); err != nil {
		return res, fmt.Errorf("refuse — no such entry: %s", link)
	}
	full, inRoot, err := resolveInRoot(root, link)
	if err != nil {
		return res, err
	}
	if !inRoot {
		return res, fmt.Errorf("refuse — %s is outside the queue root %s", link, root)
	}
	res.Link = full
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return res, fmt.Errorf("queue root unresolvable: %w", err)
	}
	rel, _ := filepath.Rel(rootReal, full)
	if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) != 3 {
		return res, fmt.Errorf("refuse — %s is not <lane>/<dir>/<ticket> under the queue root", rel)
	}
	if fi, err := os.Lstat(full); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return res, fmt.Errorf("refuse — %s is not a symlink; unlink removes overlay links only", rel)
	}

	if _, serr := os.Stat(full); serr == nil {
		storeFile, err := filepath.EvalSymlinks(full)
		if err != nil {
			return res, fmt.Errorf("resolve %s: %w", rel, err)
		}
		res.StoreFile = storeFile
		links, err := findLinksToStore(root, storeFile)
		if err != nil {
			return res, err
		}
		for _, l := range links {
			if lp, _, _ := resolveInRoot(root, l.Path); lp != full {
				res.Remaining = append(res.Remaining, l.RelPath)
			}
		}
		if len(res.Remaining) == 0 && !allowLast {
			return res, fmt.Errorf("refuse — %s is this ticket's ONLY link; removing it leaves the ticket in no lane. Move it to a valid state instead (`queue move`), or pass -allow-last", rel)
		}
	}

	if !apply {
		return res, nil
	}
	if err := os.Remove(full); err != nil {
		return res, fmt.Errorf("remove %s: %w", rel, err)
	}
	res.Removed = true
	return res, nil
}

// FormatUnlink renders an UnlinkResult for `leadkit queue unlink`.
func FormatUnlink(r UnlinkResult) string {
	var b strings.Builder
	verb := "WOULD remove"
	if r.Removed {
		verb = "removed"
	}
	fmt.Fprintf(&b, "queue-unlink: %s link %s\n", verb, r.Link)
	if r.StoreFile != "" {
		fmt.Fprintf(&b, "  ticket: %s\n", r.StoreFile)
	}
	if len(r.Remaining) > 0 {
		fmt.Fprintf(&b, "  still linked from: %s\n", strings.Join(r.Remaining, ", "))
	}
	if r.DryRun {
		b.WriteString("(dry-run — nothing removed; re-run with -apply)\n")
	}
	return b.String()
}
