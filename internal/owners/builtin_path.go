package owners

import (
	"path/filepath"
	"strings"
)

// The built-in block names the directory that grants merge authority, and a path reaches that
// directory in more spellings than the lexical one: the filesystem may fold case (APFS, the
// platform this ships on, does), a symlink anywhere on the way may alias it, and the account's HOME
// may itself be reached through a symlink (macOS /var is /private/var). The glob in
// BuiltinHardBlock is matched against the path as the caller wrote it by relFor, which resolves
// nothing; builtinBlockedPath is the same block applied to the path as the filesystem sees it, so a
// spelling the glob misses is still the protected directory.
//
// Case is folded on EVERY filesystem, by the string, because a case-sensitive one cannot be asked
// whether two spellings are the same file when one does not exist yet. The cost is that on such a
// filesystem a differently-cased sibling of the config directory is refused too; that is a refusal
// of an edit nobody should make under the account's config directory, not a bypass.
//
// Only the built-in block is normalised this way. The globs of a project's owners policy still match
// the lexical, case-sensitive REL (relFor), because they name paths in a project the owner chose
// the spelling of; ticket 53 and 157 are about those.

// builtinBlockedPath reports the BuiltinHardBlock pattern the path hits once case is folded and
// symlinks are resolved (through in.Resolve, nil for none), "" for none. rawPath is the path as the
// tool named it; it is made absolute against in.Pwd. Both the path and HOME are tried resolved and
// as written.
func builtinBlockedPath(rawPath string, in Input) string {
	if in.Home == "" {
		return ""
	}
	abs := rawPath
	if !filepath.IsAbs(abs) {
		if !filepath.IsAbs(in.Pwd) {
			return ""
		}
		abs = in.Pwd + "/" + rawPath
	}
	paths := []string{filepath.Clean(abs)}
	homes := []string{filepath.Clean(in.Home)}
	if in.Resolve != nil {
		// The unclean form too: `lnk/../x` means the parent of what lnk points at, which cleaning
		// first would turn into the parent of lnk.
		for _, p := range []string{abs, filepath.Clean(abs)} {
			if r := in.Resolve(p); r != "" {
				paths = append(paths, filepath.Clean(r))
			}
		}
		if r := in.Resolve(homes[0]); r != "" {
			homes = append(homes, filepath.Clean(r))
		}
	}
	for _, p := range paths {
		lp := strings.ToLower(p)
		for _, h := range homes {
			lh := strings.ToLower(h)
			if lh == "/" || !strings.HasPrefix(lp, lh+"/") {
				continue
			}
			rel := lp[len(lh)+1:]
			for _, pat := range BuiltinHardBlock {
				if matchGlob(strings.ToLower(pat), rel) {
					return pat
				}
			}
		}
	}
	return ""
}

// ResolvePath is the filesystem form of Input.Resolve: path with every symlink on it resolved. A path
// that does not exist yet (the file about to be created) is resolved through its deepest existing
// ancestor, with the rest appended as written. The path is shortened by its text and never cleaned on
// the way, because `lnk/../x` names the parent of what lnk points at, which cleaning first would turn
// into the parent of lnk. "" when nothing on the path can be resolved.
func ResolvePath(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	cur := strings.TrimRight(path, "/")
	rest := ""
	for cur != "" {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		slash := strings.LastIndex(cur, "/")
		base := cur[slash+1:]
		if base == ".." || base == "." {
			// A component that cannot be walked past by dropping it.
			return ""
		}
		if rest == "" {
			rest = base
		} else {
			rest = base + "/" + rest
		}
		cur = cur[:slash]
	}
	if r, err := filepath.EvalSymlinks("/"); err == nil && rest != "" {
		return filepath.Join(r, rest)
	}
	return ""
}
