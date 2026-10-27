package routing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustCreate(t *testing.T, s *Store, kind, to, status string, body []byte, now time.Time) Envelope {
	t.Helper()
	env, err := s.Create(CreateParams{
		Kind: kind, FromSlug: "sender", FromSession: "sess-a", ToSlug: to,
		Subject: "subj", Status: status, Body: body, Now: now,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return env
}

// TestCreateReadRoundTripsEnvelopeAndBody: create + read round-trips the full envelope,
// and the body_file sibling is stored and read back.
func TestCreateReadRoundTripsEnvelopeAndBody(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	env := mustCreate(t, s, "brief", "container-base", "sent", []byte("the prose body"), now)

	if env.ID == "" || env.BodyFile == "" {
		t.Fatalf("want a minted id + body_file, got %+v", env)
	}
	got, err := s.Read(env.Kind, env.ToSlug, env.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Kind != "brief" || got.FromSlug != "sender" || got.FromSession != "sess-a" ||
		got.ToSlug != "container-base" || got.Subject != "subj" || got.Status != "sent" {
		t.Errorf("envelope did not round-trip: %+v", got)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Errorf("timestamps did not round-trip: created=%s updated=%s want %s", got.CreatedAt, got.UpdatedAt, now)
	}
	if len(got.History) != 1 || got.History[0].Action != "created" {
		t.Errorf("want a single created history event, got %+v", got.History)
	}
	body, err := s.ReadBody(got)
	if err != nil || string(body) != "the prose body" {
		t.Errorf("ReadBody = %q, %v; want the stored body", body, err)
	}
}

// TestCreateRejectsInvalidKind keeps the substrate's one store-level invariant.
func TestCreateRejectsInvalidKind(t *testing.T) {
	s := Open(t.TempDir())
	for _, bad := range []string{"", "Brief", "1kind", "has space", "under_score"} {
		if _, err := s.Create(CreateParams{Kind: bad, ToSlug: "x", Now: time.Now()}); err == nil {
			t.Errorf("Create with invalid kind %q must be refused", bad)
		}
	}
	for _, ok := range []string{"brief", "gate", "a", "a-b-c", "x1"} {
		if !ValidKind(ok) {
			t.Errorf("ValidKind(%q) = false, want true", ok)
		}
	}
}

// TestAtomicWriteNeverLeavesPartialJSON: every stored .json is a complete, parseable
// envelope — the temp+rename discipline never leaves a truncated file, and no leftover
// .tmp files remain.
func TestAtomicWriteNeverLeavesPartialJSON(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Now().UTC()
	env := mustCreate(t, s, "brief", "recv", "sent", []byte("body"), now)

	// Every .json under the store parses as a full envelope; no .tmp survivors.
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".tmp") {
			t.Errorf("a temp file survived a write: %s", path)
		}
		if filepath.Ext(path) == ".json" {
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read %s: %v", path, rerr)
			}
			var e Envelope
			if jerr := json.Unmarshal(data, &e); jerr != nil {
				t.Errorf("%s is not a complete envelope (partial write?): %v", path, jerr)
			}
		}
		return nil
	})
	_ = env
}

// TestTransitionOptimisticConcurrencyRefusesStale: a transition with a STALE
// expectedStatus/updated_at is REFUSED with ErrConflict and writes NOTHING — the
// cross-session no-clobber guard. Last-writer-wins is never applied.
func TestTransitionOptimisticConcurrencyRefusesStale(t *testing.T) {
	s := Open(t.TempDir())
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	env := mustCreate(t, s, "brief", "recv", "sent", nil, t0)

	// Session A wins the race: sent -> acked.
	t1 := t0.Add(time.Minute)
	if _, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "sent", ExpectedUpdatedAt: env.UpdatedAt,
		NewStatus: "acked", Action: "ack", ActorSlug: "recv", Now: t1,
	}); err != nil {
		t.Fatalf("first transition should win: %v", err)
	}

	// Session B raced on the ORIGINAL (stale) view — must be refused, no write.
	_, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "sent", ExpectedUpdatedAt: env.UpdatedAt,
		NewStatus: "declined", Action: "decline", ActorSlug: "recv", Now: t0.Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale transition must be refused with ErrConflict, got %v", err)
	}

	// The store still reflects A's write, untouched by B.
	got, _ := s.Read(env.Kind, env.ToSlug, env.ID)
	if got.Status != "acked" {
		t.Errorf("status = %q, want acked (B must not have clobbered A)", got.Status)
	}
	if len(got.History) != 2 {
		t.Errorf("want exactly 2 history events (created + ack), got %d — B leaked a write", len(got.History))
	}

	// A stale expectedStatus alone (updated_at unchecked) is also refused.
	if _, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "sent", NewStatus: "outcome", Action: "x", Now: t1,
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("stale expectedStatus must be refused, got %v", err)
	}
}

// TestTransitionTerminalSetsClosedAt: a terminal transition marks ClosedAt; a plain one
// does not.
func TestTransitionTerminalSetsClosedAt(t *testing.T) {
	s := Open(t.TempDir())
	t0 := time.Now().UTC()
	env := mustCreate(t, s, "brief", "recv", "sent", nil, t0)

	acked, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "sent", NewStatus: "acked", Action: "ack", Now: t0.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acked.Terminal() {
		t.Error("a non-terminal transition must not set ClosedAt")
	}
	closed, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "acked", NewStatus: "closed", Terminal: true, Action: "close", Now: t0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !closed.Terminal() {
		t.Error("a terminal transition must set ClosedAt")
	}
}

// TestInboxFiltersToSlugKindStatusNewestFirst: inbox lists only items for the given slug,
// newest-first, filtered by kind/status.
func TestInboxFiltersToSlugKindStatusNewestFirst(t *testing.T) {
	s := Open(t.TempDir())
	base := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	older := mustCreate(t, s, "brief", "recv", "sent", nil, base)
	newer := mustCreate(t, s, "brief", "recv", "acked", nil, base.Add(time.Hour))
	mustCreate(t, s, "gate", "recv", "queued", nil, base.Add(2*time.Hour)) // other kind
	mustCreate(t, s, "brief", "other-slug", "sent", nil, base.Add(3*time.Hour))

	// All items for recv, newest-first.
	items, err := s.Inbox("recv", InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items for recv (not the other-slug one), got %d", len(items))
	}
	// Newest-first: gate(queued) then brief(acked) then brief(sent).
	if items[0].Kind != "gate" || items[1].ID != newer.ID || items[2].ID != older.ID {
		t.Errorf("inbox not newest-first: %v", []string{items[0].ID, items[1].ID, items[2].ID})
	}

	// Filter by kind.
	briefs, _ := s.Inbox("recv", InboxFilter{Kind: "brief"})
	if len(briefs) != 2 {
		t.Errorf("kind=brief filter: want 2, got %d", len(briefs))
	}
	// Filter by status.
	sent, _ := s.Inbox("recv", InboxFilter{Status: "sent"})
	if len(sent) != 1 || sent[0].ID != older.ID {
		t.Errorf("status=sent filter: want just the older brief, got %v", sent)
	}
}

func TestInboxJSONShapeStable(t *testing.T) {
	s := Open(t.TempDir())
	mustCreate(t, s, "brief", "recv", "sent", nil, time.Now().UTC())
	items, _ := s.Inbox("recv", InboxFilter{})
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	// The machine form (--json wraps this) must carry the stable field names.
	for _, key := range []string{`"id"`, `"kind"`, `"from_slug"`, `"to_slug"`, `"status"`, `"created_at"`, `"updated_at"`, `"history"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("inbox JSON missing stable key %s: %s", key, data)
		}
	}
}

// TestPruneNeverRemovesNonTerminal: a terminal item past the TTL is a prune candidate
// (dry-run lists, --apply removes); a NON-terminal item, at ANY age, is never pruned; and
// a dry-run mutates nothing.
func TestPruneNeverRemovesNonTerminal(t *testing.T) {
	s := Open(t.TempDir())
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ttl := 24 * time.Hour

	// A very old NON-terminal item — must NEVER be pruned.
	ancient := mustCreate(t, s, "brief", "recv", "sent", nil, now.Add(-1000*time.Hour))

	// A terminal item closed just now — under the TTL, not yet a candidate.
	fresh := mustCreate(t, s, "brief", "recv", "sent", nil, now.Add(-time.Hour))
	if _, err := s.Transition(fresh.Kind, fresh.ToSlug, fresh.ID, TransitionParams{
		ExpectedStatus: "sent", NewStatus: "closed", Terminal: true, Now: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	// A terminal item closed long ago — past the TTL, a candidate.
	stale := mustCreate(t, s, "brief", "recv", "sent", []byte("body"), now.Add(-100*time.Hour))
	if _, err := s.Transition(stale.Kind, stale.ToSlug, stale.ID, TransitionParams{
		ExpectedStatus: "sent", NewStatus: "closed", Terminal: true, Now: now.Add(-90 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Dry-run: names exactly the one stale terminal item, mutates nothing.
	res, err := s.Prune(ttl, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Removed) != 0 {
		t.Errorf("dry-run must remove nothing: %+v", res)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Env.ID != stale.ID {
		t.Fatalf("dry-run must name only the stale terminal item, got %+v", res.Candidates)
	}
	if _, err := s.Read(stale.Kind, stale.ToSlug, stale.ID); err != nil {
		t.Errorf("dry-run must not remove the stale item: %v", err)
	}

	// Apply: removes only the stale terminal item (+ its body); keeps everything else.
	res, err = s.Prune(ttl, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != stale.ID {
		t.Fatalf("apply must remove exactly the stale terminal item, got %+v", res.Removed)
	}
	if _, err := s.Read(stale.Kind, stale.ToSlug, stale.ID); err == nil {
		t.Error("apply must remove the stale terminal item")
	}
	if _, err := os.Stat(s.bodyPath(stale.Kind, stale.ToSlug, stale.ID)); !os.IsNotExist(err) {
		t.Error("apply must remove the stale item's body sibling too")
	}
	// The ancient NON-terminal + the fresh terminal survive.
	if _, err := s.Read(ancient.Kind, ancient.ToSlug, ancient.ID); err != nil {
		t.Error("a non-terminal item must NEVER be pruned, at any age")
	}
	if _, err := s.Read(fresh.Kind, fresh.ToSlug, fresh.ID); err != nil {
		t.Error("a terminal item under the TTL must not be pruned")
	}
}

// TestStaleNonTerminalFailOpen: a missing store reads clean (ok=true, empty), never an
// error — doctor's routing-inbox check leans on this.
func TestStaleNonTerminalFailOpen(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "does-not-exist"))
	items, ok := s.StaleNonTerminal(time.Hour, time.Now())
	if !ok || len(items) != 0 {
		t.Errorf("missing store: want ok=true empty, got ok=%v items=%v", ok, items)
	}
}

// TestList_FiltersAcrossRecipientsOnly pins List as the recipient-less Inbox: it spans
// recipients, and applies the kind and status filters rather than returning everything.
func TestList_FiltersAcrossRecipientsOnly(t *testing.T) {
	s := Open(t.TempDir())
	mk := func(kind, to, status string, at time.Time) Envelope {
		t.Helper()
		e, err := s.Create(CreateParams{Kind: kind, FromSlug: "f", ToSlug: to, Subject: kind + "/" + to + "/" + status, Status: status, Now: at})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := mk("brief", "a", "parked", t0.Add(2*time.Minute))
	b := mk("brief", "b", "parked", t0.Add(1*time.Minute))
	mk("brief", "a", "sent", t0)
	mk("gate", "a", "parked", t0)

	got, err := s.List(InboxFilter{Kind: "brief", Status: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("want exactly the two parked briefs, oldest-updated first (%s, %s), got %+v", b.ID, a.ID, got)
	}
	all, _ := s.List(InboxFilter{})
	if len(all) != 4 {
		t.Fatalf("an empty filter lists everything, got %d", len(all))
	}
	if none, _ := Open(t.TempDir() + "/missing").List(InboxFilter{}); len(none) != 0 {
		t.Fatalf("a missing store is empty, got %+v", none)
	}
}

// TestCreate_ParentIsRecordedAndOptional: a derived item remembers where it came from, and
// an item that was not derived serialises with no parent key at all - so the field is
// additive and an envelope written before it existed reads back unchanged.
func TestCreate_ParentIsRecordedAndOptional(t *testing.T) {
	s := Open(t.TempDir())
	plain, err := s.Create(CreateParams{Kind: "brief", FromSlug: "a", ToSlug: "b", Subject: "plain", Status: "sent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Create(CreateParams{Kind: "brief", FromSlug: "b", ToSlug: "c", Subject: "child", Status: "sent", Parent: plain.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Locate("brief", child.ID)
	if err != nil || got.Parent != plain.ID {
		t.Fatalf("a derived item must keep its parent id %q, got %q (err %v)", plain.ID, got.Parent, err)
	}
	if again, _ := s.Locate("brief", plain.ID); again.Parent != "" {
		t.Fatalf("an item that was not derived must have no parent, got %q", again.Parent)
	}
	raw, err := os.ReadFile(s.jsonPath("brief", "b", plain.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"parent"`) {
		t.Fatalf("a non-derived envelope must omit the parent key, got:\n%s", raw)
	}
}
