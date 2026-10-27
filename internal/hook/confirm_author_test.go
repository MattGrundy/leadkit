package hook

import (
	"testing"
)

// ConfirmPRAuthor is the exported owner_prs_only check the governed `leadkit pr edit`
// path reuses. It must apply the SAME fail-closed semantics as PRGuard's raw-edit
// branch, driven by the SAME prAuthorLookup seam (stubbed here — never a real gh/glab).

func TestConfirmPRAuthor_Match_OK(t *testing.T) {
	stubAuthor(t, "me", "me", true)
	ok, reason := ConfirmPRAuthor("github", "/repo", "", "42")
	if !ok {
		t.Fatalf("author==self==ok should confirm, got reason=%q", reason)
	}
}

func TestConfirmPRAuthor_DifferentAuthor_FailsClosed(t *testing.T) {
	stubAuthor(t, "someone-else", "me", true)
	if ok, _ := ConfirmPRAuthor("github", "/repo", "", "42"); ok {
		t.Fatalf("a different author must NOT confirm (fail-closed)")
	}
}

func TestConfirmPRAuthor_LookupFailed_FailsClosed(t *testing.T) {
	stubAuthor(t, "", "", false) // lookup failed / offline
	if ok, _ := ConfirmPRAuthor("gitlab", "/repo", "", "42"); ok {
		t.Fatalf("a failed lookup must NOT confirm (fail-closed)")
	}
}

func TestConfirmPRAuthor_EmptyLogins_FailsClosed(t *testing.T) {
	// ok=true but empty logins (a parse that yielded nothing) must still fail closed.
	stubAuthor(t, "", "", true)
	if ok, _ := ConfirmPRAuthor("github", "/repo", "", "42"); ok {
		t.Fatalf("empty logins must NOT confirm (fail-closed)")
	}
}

func TestConfirmPRAuthor_UnknownForge_FailsClosed(t *testing.T) {
	stubAuthor(t, "me", "me", true) // even a would-be match
	if ok, _ := ConfirmPRAuthor("bitbucket", "/repo", "", "42"); ok {
		t.Fatalf("an unknown forge must NOT confirm (fail-closed)")
	}
}
