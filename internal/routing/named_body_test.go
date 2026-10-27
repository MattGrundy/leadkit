package routing

import (
	"errors"
	"testing"
	"time"
)

// TestLocate_FindsByIDWithoutKnowingToSlug: the sender path — Locate resolves an item by
// (kind, id) without the caller supplying the to_slug segment it is filed under.
func TestLocate_FindsByIDWithoutKnowingToSlug(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now().UTC()
	env, err := s.Create(CreateParams{Kind: "brief", FromSlug: "sender", ToSlug: "recv", Subject: "s", Status: "sent", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Locate("brief", env.ID)
	if err != nil {
		t.Fatalf("Locate must find the item: %v", err)
	}
	if got.ID != env.ID || got.ToSlug != "recv" {
		t.Fatalf("Locate returned the wrong item: %+v", got)
	}
	if _, err := s.Locate("brief", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Locate on an unknown id must be ErrNotFound, got %v", err)
	}
	if _, err := s.Locate("gate", env.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Locate under the wrong kind must be ErrNotFound, got %v", err)
	}
}

// TestTransition_AttachesNamedSecondaryBody: a transition can durably attach a named
// secondary body (brief's outcome), readable back via ReadNamedBody, recorded in Bodies.
func TestTransition_AttachesNamedSecondaryBody(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now().UTC()
	env, err := s.Create(CreateParams{Kind: "brief", ToSlug: "recv", Status: "acked", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	env, err = s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "acked", NewStatus: "outcome", Action: "outcome",
		AttachBodies: map[string][]byte{"outcome": []byte("the result")}, Now: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("transition with an attached body: %v", err)
	}
	if env.Bodies["outcome"] != env.ID+".outcome.md" {
		t.Fatalf("Bodies must record the outcome sibling name, got %v", env.Bodies)
	}
	data, err := s.ReadNamedBody(env, "outcome")
	if err != nil || string(data) != "the result" {
		t.Fatalf("ReadNamedBody must return the stored outcome, got %q err=%v", data, err)
	}
	// An unknown body name reads as (nil, nil), not an error.
	if data, err := s.ReadNamedBody(env, "missing"); err != nil || data != nil {
		t.Fatalf("ReadNamedBody for an absent name must be (nil,nil), got %q err=%v", data, err)
	}
}

// TestTransition_RejectsInvalidBodyName: a bad secondary-body name is refused (path safety)
// and nothing is written.
func TestTransition_RejectsInvalidBodyName(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now().UTC()
	env, _ := s.Create(CreateParams{Kind: "brief", ToSlug: "recv", Status: "acked", Now: now})
	_, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "acked", NewStatus: "outcome", Action: "outcome",
		AttachBodies: map[string][]byte{"../evil": []byte("x")}, Now: now,
	})
	if err == nil {
		t.Fatal("an invalid body name must be refused")
	}
	// The status must be unchanged (no partial write).
	reread, _ := s.Read(env.Kind, env.ToSlug, env.ID)
	if reread.Status != "acked" {
		t.Fatalf("a refused body attach must leave the status unchanged, got %q", reread.Status)
	}
}

// TestPrune_RemovesNamedBodies: retention removes an item's named secondary bodies along
// with its envelope + primary body.
func TestPrune_RemovesNamedBodies(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Now().UTC()
	env, _ := s.Create(CreateParams{Kind: "brief", ToSlug: "recv", Status: "acked", Body: []byte("brief"), Now: now.Add(-100 * time.Hour)})
	env, _ = s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "acked", NewStatus: "outcome", Action: "outcome",
		AttachBodies: map[string][]byte{"outcome": []byte("result")}, Now: now.Add(-99 * time.Hour),
	})
	env, _ = s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "outcome", NewStatus: "closed", Terminal: true, Action: "closed", Now: now.Add(-98 * time.Hour),
	})
	res, err := s.Prune(time.Hour, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 {
		t.Fatalf("the terminal item must be removed, got %v", res.Removed)
	}
	// The outcome sibling must be gone too.
	if _, err := s.ReadNamedBody(env, "outcome"); err == nil {
		t.Fatal("prune must remove the named secondary body file")
	}
}
