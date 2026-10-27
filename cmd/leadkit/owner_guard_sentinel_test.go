package main

// owner-guard sentinels live under the project `.leadkit/` folder (the leadkit-owned
// namespace), NOT the bare repo root. These unit tests exercise buildOwnerGuardConfig
// directly (no real binary, no real ~/.claude) and assert the `.leadkit/` paths
// specifically — a root-level `.bypass` must NOT trigger bypass any more.

import (
	"os"
	"path/filepath"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/owners"
)

// rootedProfile writes a minimal leadkit.yaml into root and loads a profile rooted
// there, so prof.Resolve(".leadkit/...") points inside root. HOME is redirected to an
// empty temp dir so the user config layer can't leak the real machine's settings.
func rootedProfile(t *testing.T, root string) *config.Profile {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	profPath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profPath, []byte("queue_root: queue\nstore_root: tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", profPath)
	prof, _, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return prof
}

func writeLeadkitFile(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, ".leadkit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildOwnerGuardConfig_BypassFromLeadkitDir(t *testing.T) {
	t.Run("present under .leadkit → bypass true", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		writeLeadkitFile(t, root, "bypass", "")
		cfg := buildOwnerGuardConfig(prof)
		if !cfg.Bypass {
			t.Error("want Bypass=true when .leadkit/bypass is present")
		}
	})

	t.Run("absent → bypass false", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		cfg := buildOwnerGuardConfig(prof)
		if cfg.Bypass {
			t.Error("want Bypass=false when no .leadkit/bypass sentinel exists")
		}
	})

	t.Run("root-level .bypass no longer triggers bypass", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		// The OLD location — a bare repo-root .bypass. It must NOT be honored now.
		if err := os.WriteFile(filepath.Join(root, ".bypass"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := buildOwnerGuardConfig(prof)
		if cfg.Bypass {
			t.Error("a root-level .bypass must NOT trigger bypass — the sentinel lives under .leadkit/ now")
		}
	})
}

func TestBuildOwnerGuardConfig_CallerPrecedence(t *testing.T) {
	t.Run("caller from .leadkit/active-agent", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		writeLeadkitFile(t, root, "active-agent", "fe-agent\n")
		cfg := buildOwnerGuardConfig(prof)
		if cfg.Caller != "fe-agent" {
			t.Errorf("Caller = %q, want fe-agent (from .leadkit/active-agent)", cfg.Caller)
		}
	})

	t.Run("env wins over active-agent", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "env-agent")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		writeLeadkitFile(t, root, "active-agent", "fe-agent\n")
		cfg := buildOwnerGuardConfig(prof)
		if cfg.Caller != "env-agent" {
			t.Errorf("Caller = %q, want env-agent (CLAUDE_AGENT_NAME wins)", cfg.Caller)
		}
	})

	t.Run("falls back to lead when both absent", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		cfg := buildOwnerGuardConfig(prof)
		if cfg.Caller != owners.DefaultLead {
			t.Errorf("Caller = %q, want the lead default %q", cfg.Caller, owners.DefaultLead)
		}
	})

	t.Run("a root-level .active-agent is ignored", func(t *testing.T) {
		t.Setenv("CLAUDE_AGENT_NAME", "")
		root := t.TempDir()
		prof := rootedProfile(t, root)
		// OLD location — must NOT be read.
		if err := os.WriteFile(filepath.Join(root, ".active-agent"), []byte("stale-agent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := buildOwnerGuardConfig(prof)
		if cfg.Caller != owners.DefaultLead {
			t.Errorf("Caller = %q, want the lead default — a root-level .active-agent must be ignored", cfg.Caller)
		}
	})
}

func TestBuildOwnerGuardConfig_AuditLogUnderLeadkitDir(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "")
	root := t.TempDir()
	prof := rootedProfile(t, root)
	cfg := buildOwnerGuardConfig(prof)
	want := filepath.Join(root, ".leadkit", "owner-guard.log")
	if cfg.AuditLogPath != want {
		t.Errorf("AuditLogPath = %q, want %q", cfg.AuditLogPath, want)
	}
}
