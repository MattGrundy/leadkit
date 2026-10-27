package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The account-level burn-enforce daemon (docs/build-tasks/burn-auto-enforce.md).

func TestComputeBurnDaemonPlan_Darwin_AccountScopedUnsluggedWithWorkingDir(t *testing.T) {
	dir := t.TempDir()
	plan := ComputeBurnDaemonPlan("darwin", "/abs/leadkit", "/home/u/.claude/leadkit", "/home/u/.claude/leadkit", dir, 120)
	if !plan.Supported || len(plan.Services) != 1 {
		t.Fatalf("want 1 supported service, got supported=%v services=%d", plan.Supported, len(plan.Services))
	}
	svc := plan.Services[0]
	if svc.Spec.Label != BurnLabel {
		t.Errorf("label = %q, want the un-slugged account label %q", svc.Spec.Label, BurnLabel)
	}
	if filepath.Base(svc.Spec.PlistPath) != BurnLabel+".plist" {
		t.Errorf("plist filename = %q, want %q", filepath.Base(svc.Spec.PlistPath), BurnLabel+".plist")
	}
	plist := string(svc.Spec.Plist)
	for _, want := range []string{
		"<string>/abs/leadkit</string>",
		"<string>burn</string>",
		"<string>enforce</string>",
		"<key>WorkingDirectory</key>",
		"<string>/home/u/.claude/leadkit</string>",
		"<key>StartInterval</key>",
		"<integer>120</integer>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("burn plist missing %q:\n%s", want, plist)
		}
	}
}

func TestComputeBurnDaemonPlan_NonDarwin_Skips(t *testing.T) {
	plan := ComputeBurnDaemonPlan("linux", "/abs/leadkit", "/l", "/l", t.TempDir(), 120)
	if plan.Supported || plan.SkipNote == "" {
		t.Fatalf("non-darwin should be unsupported with a skip note, got %+v", plan)
	}
}

func TestComputeBurnDaemonPlan_IdempotentWhenIdentical(t *testing.T) {
	dir := t.TempDir()
	plan := ComputeBurnDaemonPlan("darwin", "/abs/leadkit", "/l", "/l", dir, 120)
	if err := os.WriteFile(plan.Services[0].Spec.PlistPath, plan.Services[0].Spec.Plist, 0o644); err != nil {
		t.Fatal(err)
	}
	again := ComputeBurnDaemonPlan("darwin", "/abs/leadkit", "/l", "/l", dir, 120)
	if again.Services[0].Changed {
		t.Error("identical existing plist should be Changed=false (idempotent)")
	}
}
