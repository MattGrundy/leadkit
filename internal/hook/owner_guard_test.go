package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/owners"
)

func ownerPolicy() *owners.Policy {
	return &owners.Policy{
		HardBlock: []string{"secrets/**"},
		Paths:     []owners.PathOwner{{Glob: "Projects/repo/frontend/**", Owner: "fe-agent"}},
	}
}

// staticPolicy is the trivial injected resolver most owner-guard tests use: it returns the
// SAME policy for every target path, driving OwnerGuard through the injected ResolvePolicy
// seam without a real profile on disk (the real cmd/leadkit resolver loads the TARGET
// file's project owners_file — see the cross-project test).
func staticPolicy(p *owners.Policy) func(string) (*owners.Policy, bool) {
	return func(string) (*owners.Policy, bool) { return p, true }
}

func ownerCfg(logPath string) OwnerGuardConfig {
	return OwnerGuardConfig{
		ResolvePolicy: staticPolicy(ownerPolicy()),
		Caller:        "fe-agent",
		Lead:          "lead",
		Home:          "/home/me",
		Pwd:           "/home/me",
		AuditLogPath:  logPath,
	}
}

func ownerEdit(path string) Input {
	return Input{ToolName: "Edit", ToolInput: toolInput{FilePath: "/home/me/" + path}}
}

func TestOwnerGuard_DisabledWhenPolicyNotLoaded(t *testing.T) {
	// A resolver that reports loaded=false (no owners_file / absent / unparseable) disables
	// the boundary → allow, even for a would-be hard-blocked path.
	cfg := OwnerGuardConfig{
		ResolvePolicy: func(string) (*owners.Policy, bool) { return nil, false },
		Home:          "/home/me", Pwd: "/home/me",
	}
	if got := OwnerGuard(ownerEdit("secrets/key.pem"), cfg); got != allow {
		t.Errorf("no policy loaded → boundary disabled → allow, got %+v", got)
	}
	// A nil resolver is likewise disabled (fail open), never a panic or an allow-all deny.
	nilCfg := OwnerGuardConfig{Home: "/home/me", Pwd: "/home/me"}
	if got := OwnerGuard(ownerEdit("secrets/key.pem"), nilCfg); got != allow {
		t.Errorf("nil resolver → boundary disabled → allow, got %+v", got)
	}
}

func TestOwnerGuard_DenyMapsToExit2WithStderr(t *testing.T) {
	got := OwnerGuard(ownerEdit("Projects/repo/frontend/app.tsx"), OwnerGuardConfig{
		ResolvePolicy: staticPolicy(ownerPolicy()), Caller: "backend-agent", Lead: "lead", Home: "/home/me", Pwd: "/home/me",
	})
	if got.ExitCode != 2 || got.Stderr == "" {
		t.Errorf("wrong-owner edit = %+v, want exit 2 with a stderr deny", got)
	}
	if !strings.Contains(got.Stderr, "owned by 'fe-agent'") {
		t.Errorf("stderr = %q, want the handoff message", got.Stderr)
	}
}

func TestOwnerGuard_OwnerAllowedAndAudited(t *testing.T) {
	log := filepath.Join(t.TempDir(), "owner-guard.log")
	if got := OwnerGuard(ownerEdit("Projects/repo/frontend/app.tsx"), ownerCfg(log)); got != allow {
		t.Errorf("owner on own path = %+v, want allow", got)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("audit log not written: %v", err)
	}
	if !strings.Contains(string(data), "ALLOW-OWNED") || !strings.Contains(string(data), "fe-agent") {
		t.Errorf("audit line = %q, want an ALLOW-OWNED record for fe-agent", data)
	}
}

func TestOwnerGuard_BashDeletionDenied(t *testing.T) {
	got := OwnerGuard(Input{ToolName: "Bash", ToolInput: toolInput{Command: "rm Projects/repo/frontend/app.tsx"}}, OwnerGuardConfig{
		ResolvePolicy: staticPolicy(ownerPolicy()), Caller: "backend-agent", Lead: "lead", Home: "/home/me", Pwd: "/home/me",
	})
	if got.ExitCode != 2 {
		t.Errorf("cross-boundary rm = %+v, want deny", got)
	}
}

// TestOwnerGuard_ResolvesPolicyFromTargetFile proves the cross-project fix: the policy is
// resolved from the TARGET edit path, not an ambient field. The resolver returns an owning
// policy ONLY for the target project's tree (as cmd/leadkit's resolveTargetOwnersPolicy
// would, loading THAT project's owners_file) — a non-owner editing an owned file in the
// target project is DENIED even though the acting session is foreign; the owner is allowed;
// and a file in a project that defines no owners_file (resolver → loaded=false) is
// allow-unowned. The resolver also proves it receives the target edit path.
func TestOwnerGuard_ResolvesPolicyFromTargetFile(t *testing.T) {
	const owned = "/home/me/Projects/target-proj/frontend/app.tsx"
	const unowned = "/home/me/Projects/no-owners-proj/x.tsx"
	targetPolicy := &owners.Policy{
		Paths: []owners.PathOwner{{Glob: "Projects/target-proj/frontend/**", Owner: "fe-agent"}},
	}
	var gotTarget string
	resolve := func(target string) (*owners.Policy, bool) {
		gotTarget = target
		if strings.Contains(target, "target-proj") {
			return targetPolicy, true // the target project defines ownership
		}
		return nil, false // the other project defines none → disabled
	}

	// A foreign session (backend-agent) editing target-proj's fe-owned file → DENY.
	deny := OwnerGuard(Input{ToolName: "Edit", ToolInput: toolInput{FilePath: owned}}, OwnerGuardConfig{
		ResolvePolicy: resolve, Caller: "backend-agent", Lead: "lead", Home: "/home/me", Pwd: "/home/me",
	})
	if deny.ExitCode != 2 {
		t.Errorf("non-owner editing target-proj's owned file = %+v, want DENY (boundary enforced cross-project)", deny)
	}
	if gotTarget != owned {
		t.Errorf("resolver got target=%q, want the edit path %q", gotTarget, owned)
	}
	// The owner editing the same file → allow.
	ok := OwnerGuard(Input{ToolName: "Edit", ToolInput: toolInput{FilePath: owned}}, OwnerGuardConfig{
		ResolvePolicy: resolve, Caller: "fe-agent", Lead: "lead", Home: "/home/me", Pwd: "/home/me",
	})
	if ok != allow {
		t.Errorf("owner editing own file = %+v, want allow", ok)
	}
	// A file in a project with no owners_file → allow-unowned (unchanged).
	unownedRes := OwnerGuard(Input{ToolName: "Edit", ToolInput: toolInput{FilePath: unowned}}, OwnerGuardConfig{
		ResolvePolicy: resolve, Caller: "backend-agent", Lead: "lead", Home: "/home/me", Pwd: "/home/me",
	})
	if unownedRes != allow {
		t.Errorf("edit in a project defining no owners = %+v, want allow-unowned", unownedRes)
	}
}

// TestOwnerGuard_CloneFamiliesFromTargetPolicy proves clone_families come from the TARGET
// project's policy: the target policy canonicalizes its own clone prefix, so an edit under
// a renamed clone of the owned tree is collapsed onto the canonical owner and DENIED for a
// non-owner — no cross-namespace canonicalize false-deny, because the families are the
// target's own.
func TestOwnerGuard_CloneFamiliesFromTargetPolicy(t *testing.T) {
	const cloneEdit = "/home/me/Projects/target-proj-fe/frontend/app.tsx" // a clone prefix
	targetPolicy := &owners.Policy{
		Paths:         []owners.PathOwner{{Glob: "Projects/target-proj/frontend/**", Owner: "fe-agent"}},
		CloneFamilies: map[string][]string{"target-proj": {"target-proj-fe"}},
	}
	res := OwnerGuard(Input{ToolName: "Edit", ToolInput: toolInput{FilePath: cloneEdit}}, OwnerGuardConfig{
		ResolvePolicy: staticPolicy(targetPolicy), Caller: "backend-agent", Lead: "lead", Home: "/home/me", Pwd: "/home/me",
	})
	if res.ExitCode != 2 {
		t.Errorf("non-owner editing a clone-family path of the owned tree = %+v, want DENY", res)
	}
}
