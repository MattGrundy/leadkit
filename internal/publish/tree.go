package publish

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ParseManifest reads the exclusion manifest: one repository-relative path per line (a file or a
// directory, which is removed with everything under it), `//` comments and blank lines ignored.
// It uses the same comment marker as the scrub term file, so the two files read alike. A path
// that is absolute, climbs out of the tree, or holds a glob character is an error: a pattern the
// tool cannot honour would silently exclude nothing.
func ParseManifest(data string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for n, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.ContainsAny(line, `*?[\`) {
			return nil, fmt.Errorf("line %d: %q holds a glob or escape character; list exact paths", n+1, line)
		}
		clean := path.Clean(line)
		if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("line %d: %q is not a path inside the tree", n+1, line)
		}
		if clean == ".git" || strings.HasPrefix(clean, ".git/") {
			return nil, fmt.Errorf("line %d: %q is repository metadata, not tree content", n+1, line)
		}
		if !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the manifest lists no paths; an empty exclusion list is not a list")
	}
	return out, nil
}

// extractTar unpacks a tar stream into dest, which must exist. Entries that would land outside
// dest, or write through a symlink, are an error.
func extractTar(data []byte, dest string) error {
	root, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := path.Clean(h.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive entry %q escapes the destination", h.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		parent, err := resolveParent(root, target)
		if err != nil {
			return fmt.Errorf("archive entry %q: %w", h.Name, err)
		}
		target = filepath.Join(parent, filepath.Base(target))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, body, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has an unsupported type %q", h.Name, string(h.Typeflag))
		}
	}
}

// resolveParent returns target's parent directory with symlinks resolved, and errors when that is
// not inside root. Components that do not exist yet are taken as written.
func resolveParent(root, target string) (string, error) {
	parent := filepath.Dir(target)
	cur := root
	rel, err := filepath.Rel(root, parent)
	if err != nil {
		return "", err
	}
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			next := filepath.Join(cur, part)
			fi, lerr := os.Lstat(next)
			if errors.Is(lerr, fs.ErrNotExist) {
				cur = next
				continue
			}
			if lerr != nil {
				return "", lerr
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return "", errors.New("writes through a symlink")
			}
			cur = next
		}
	}
	return cur, nil
}

// applyManifest removes each listed path from root and returns the ones that were not there.
// A listed path that matches nothing is a STALE entry: a file that was moved or deleted without
// the manifest being told is an exclusion the list only appears to make.
func applyManifest(root string, paths []string) (stale []string, err error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		full := filepath.Join(realRoot, filepath.FromSlash(p))
		// A committed symlink in the middle of the path would carry the removal outside the tree
		// (a file elsewhere on this machine). Refuse rather than follow it, as extraction does.
		if _, perr := resolveParent(realRoot, full); perr != nil {
			return nil, fmt.Errorf("manifest path %q: %w; nothing was removed outside the tree", p, perr)
		}
		if _, lerr := os.Lstat(full); errors.Is(lerr, fs.ErrNotExist) {
			stale = append(stale, p)
			continue
		} else if lerr != nil {
			return nil, lerr
		}
		if err := os.RemoveAll(full); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// listTree returns the sorted slash-separated paths of every file and symlink under root.
func listTree(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// copyTree copies every file and symlink under src into dst, creating directories, keeping the
// executable bit. It never writes over anything: an existing file or link at a target is an error
// (O_EXCL does not follow a symlink either), and an existing directory must be a real one, so a
// link in dst cannot carry a write outside it.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		to := filepath.Join(dst, rel)
		if fi, lerr := os.Lstat(to); lerr == nil {
			if !(d.IsDir() && fi.IsDir()) {
				return fmt.Errorf("refusing to write over existing %s", to)
			}
			return nil
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return lerr
		}
		switch {
		case d.IsDir():
			return os.Mkdir(to, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			return os.Symlink(target, to)
		default:
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			mode := os.FileMode(0o644)
			if fi, serr := d.Info(); serr == nil && fi.Mode()&0o111 != 0 {
				mode = 0o755
			}
			f, oerr := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if oerr != nil {
				return oerr
			}
			if _, werr := f.Write(data); werr != nil {
				_ = f.Close()
				return werr
			}
			return f.Close()
		}
	})
}
