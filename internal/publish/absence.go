package publish

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// LoadManifest reads and parses the exclusion manifest at DefaultManifest under root.
func LoadManifest(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(DefaultManifest)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", DefaultManifest, err)
	}
	paths, err := ParseManifest(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", DefaultManifest, err)
	}
	return paths, nil
}

// Excluded reports whether rel, a slash-separated repository-relative path (a trailing slash is
// ignored), is a manifest path or lies under one. It is how a test tells a file that publish
// legitimately removed from one that went missing: absence is explained only by being listed.
func Excluded(manifest []string, rel string) bool {
	rel = path.Clean(strings.TrimSuffix(rel, "/"))
	for _, p := range manifest {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}
