package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/install"
)

func TestPrePushHookCheck_EachStateSaysWhatItIs(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "leadkit")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "/repo/.git/hooks/pre-push"
	for name, c := range map[string]struct {
		st     install.PrePushStatus
		ok     bool
		inText []string
	}{
		"installed": {install.PrePushStatus{State: install.PrePushInstalled, HookPath: hook, Binary: bin, Detail: hook + " runs " + bin}, true,
			[]string{"installed", "--no-verify"}},
		"installed, binary gone": {install.PrePushStatus{State: install.PrePushInstalled, HookPath: hook, Binary: "/gone/leadkit"}, false,
			[]string{"REFUSES every push", "/gone/leadkit", "leadkit init"}},
		"missing": {install.PrePushStatus{State: install.PrePushMissing, Detail: "no pre-push hook at " + hook}, false,
			[]string{"NOT installed", "text hook", "leadkit init"}},
		"foreign": {install.PrePushStatus{State: install.PrePushForeign, Detail: hook + " exists and is not leadkit's"}, false,
			[]string{"NOT WIRED", "gate check-push"}},
		"stale":          {install.PrePushStatus{State: install.PrePushStale, Detail: "differs"}, false, []string{"NOT WIRED", "leadkit init"}},
		"not executable": {install.PrePushStatus{State: install.PrePushNotExecutable, Detail: "not executable"}, false, []string{"NOT WIRED"}},
		"redirected": {install.PrePushStatus{State: install.PrePushRedirected, Detail: "core.hooksPath is set to /elsewhere (file:/home/x/.gitconfig)"}, false,
			[]string{"NOT WIRED, redirected", "/elsewhere", "gitconfig"}},
		"not a repo": {install.PrePushStatus{State: install.PrePushNoRepo, Detail: "/tmp is not inside a git work tree"}, true, []string{"not checked"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := PrePushHookCheck(c.st)
			if got.Name != "pre-push-hook" || got.OK != c.ok {
				t.Fatalf("got %+v, want ok=%v", got, c.ok)
			}
			if !got.OK && got.Severity != SeverityWarn {
				t.Errorf("a degraded layer is a warning, never critical: %v", got.Severity)
			}
			for _, w := range c.inText {
				if !strings.Contains(got.Detail, w) {
					t.Errorf("detail lacks %q: %s", w, got.Detail)
				}
			}
		})
	}
}
