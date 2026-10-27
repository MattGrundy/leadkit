// Command gen-config-reference regenerates docs/CONFIG-REFERENCE.md from the config
// schema — the durable, committed form of `leadkit config reference --markdown`.
// Invoked by `go generate ./internal/config` (see internal/config/reference.go). It
// walks up from the working dir to the module root (the dir holding go.mod) so it
// writes the doc to the right place regardless of where go generate runs it.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/config"
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-config-reference: %v\n", err)
		os.Exit(1)
	}
	out := filepath.Join(root, "docs", "CONFIG-REFERENCE.md")
	content := config.FormatReferenceMarkdown(config.Reference())
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-config-reference: write %s: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", out)
}

// moduleRoot walks up from cwd to the first dir containing go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found walking up from working dir")
		}
		dir = parent
	}
}
