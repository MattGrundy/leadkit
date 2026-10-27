package review

import (
	"fmt"
	"os"
	"strings"
)

// KV is one frontmatter key/value to set.
type KV struct{ Key, Value string }

// UpdateFrontmatter rewrites path's leading "---\n...\n---\n" frontmatter block,
// setting each key in updates. An existing "key: value" line is replaced in place
// (preserving its position and every other line's position/order); a key not already
// present is appended, in updates order, just before the closing "---". Everything
// outside the block, and every frontmatter line whose key isn't in updates, is left
// untouched. It runs under the ticket lock, so it cannot clobber a note appended by
// AppendNote between its read and its write.
func UpdateFrontmatter(path string, updates []KV) error {
	return withTicketLock(path, func() error { return updateFrontmatterLocked(path, updates) })
}

func updateFrontmatterLocked(path string, updates []KV) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("review: read %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return fmt.Errorf("review: %s has no opening frontmatter delimiter", path)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return fmt.Errorf("review: %s frontmatter block never closes", path)
	}

	pending := map[string]string{}
	for _, kv := range updates {
		pending[kv.Key] = kv.Value
	}

	var fm []string
	for _, line := range lines[1:end] {
		if key, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			key = strings.TrimSpace(key)
			if val, want := pending[key]; want {
				fm = append(fm, fmt.Sprintf("%s: %s", key, val))
				delete(pending, key)
				continue
			}
		}
		fm = append(fm, line)
	}
	// Append keys that had no existing line, in the caller's given order.
	for _, kv := range updates {
		if _, stillPending := pending[kv.Key]; stillPending {
			fm = append(fm, fmt.Sprintf("%s: %s", kv.Key, kv.Value))
			delete(pending, kv.Key)
		}
	}

	out := append([]string{"---"}, fm...)
	out = append(out, lines[end:]...)
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return fmt.Errorf("review: write %s: %w", path, err)
	}
	return nil
}

// ReadFrontmatter parses path's "key: value" frontmatter block into a map. Only the
// FIRST value for a repeated key is kept (files here don't repeat keys in practice).
func ReadFrontmatter(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("review: read %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("review: %s has no opening frontmatter delimiter", path)
	}
	out := map[string]string{}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return out, nil
		}
		key, val, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, seen := out[key]; !seen {
			out[key] = strings.TrimSpace(val)
		}
	}
	return nil, fmt.Errorf("review: %s frontmatter block never closes", path)
}
