// Package routing is leadkit's account-level, project-addressed, status-tracked
// cross-session ITEM store (docs/build-tasks/routing-substrate.md, build 1 of
// docs/design/account-routing-brief-central-pool.md). It is the durable foundation the
// `brief` (build 2) and `gate`/central-pool (build 3) features sit on — this layer is
// deliberately KIND-AGNOSTIC: it stores, addresses, transitions and retains items
// generically, and knows nothing of any specific kind's lifecycle or vocabulary. No
// `brief`/`gate` specifics ever leak in here.
//
// STORE LAYOUT: ~/.claude/leadkit/routing/<kind>/<to-slug>/<id>.json — the JSON
// envelope — with an optional sibling <id>.md carrying a human-authored prose body.
// <kind> is a free-ish label (validated [a-z][a-z0-9-]*) so a new kind never touches
// this package.
//
// WRITE DISCIPLINE (mirrors internal/queue + the presence/project registries, never
// forks them): every write — create AND transition — is ATOMIC (write <id>.json.tmp in
// the same dir, then os.Rename over <id>.json), so a reader never observes a partial
// file. Writes are single-writer-COORDINATE across sessions the way the queue is
// single-writer per lane: an out-of-order transition is refused, not silently applied
// (see Transition's optimistic-concurrency guard). Reads never block writes.
//
// This package NEVER sends a cross-session nudge — Go cannot SendMessage (that is an
// agent tool). Delivery is the durable write plus a printed presence-derived HINT (see
// delivery.go); the sending AGENT does the actual nudge. Nothing here imports or invokes
// any messaging primitive.
package routing

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Event is one append-only audit entry on an item's history — who did what, when. The
// history travels WITH the item (same file), so both sides and `doctor` see the full
// trail, and retention prunes the trail together with the item.
type Event struct {
	TS        time.Time `json:"ts"`
	BySlug    string    `json:"by_slug"`
	BySession string    `json:"by_session"`
	Action    string    `json:"action"`
	Note      string    `json:"note,omitempty"`
}

// Envelope is the common item record shared by every kind. `Status` is an OPAQUE string
// here: each kind defines its own lifecycle in build 2/3; the substrate only stores and
// transitions it. `ClosedAt` is the substrate's kind-agnostic TERMINAL marker — a
// Transition sets it when the caller says the new status is terminal, and retention
// only ever prunes an item that carries it (see Prune). `BodyFile` is the optional
// sibling prose file's base name (relative to the item's own directory). `Bodies` is
// the kind-agnostic NAMED-SECONDARY-BODY map (logical name -> sibling base name, e.g.
// "outcome" -> "<id>.outcome.md"): the primary `BodyFile` carries the item's original
// prose, and a kind that grows a second durable prose deliverable (brief's outcome
// record) attaches it here without this layer learning that kind's vocabulary. A body
// is written to disk BEFORE the envelope references it (see Transition), so a reader
// that sees a Bodies entry always finds its file present.
type Envelope struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	FromSlug    string            `json:"from_slug"`
	FromSession string            `json:"from_session,omitempty"`
	ToSlug      string            `json:"to_slug"`
	Subject     string            `json:"subject"`
	Status      string            `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ClosedAt    *time.Time        `json:"closed_at,omitempty"`
	BodyFile    string            `json:"body_file,omitempty"`
	Bodies      map[string]string `json:"bodies,omitempty"`
	// Parent is the id of the item this one was DERIVED from (e.g. a routed brief's
	// escalated original). It is a plain id link, kind-agnostic and additive: an older
	// reader ignores it, and it is empty for every item that was not derived. The child
	// is a separate item with its own lifecycle; Parent only lets a reader walk back.
	Parent  string  `json:"parent,omitempty"`
	History []Event `json:"history"`
}

// Terminal reports whether the item has reached a kind-terminal state — the substrate's
// generic definition being "a Transition marked it closed" (ClosedAt set). Retention
// keys on this and nothing else, so it can never prune a live item.
func (e Envelope) Terminal() bool { return e.ClosedAt != nil }

// kindRe validates a <kind> label: lowercase-first, then lowercase/digits/hyphens. Keeps
// the store path a safe single segment (no traversal, no surprise separators) and lets
// build 2/3 add `brief`/`gate` without this layer changing.
var kindRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidKind reports whether kind is a legal <kind> label.
func ValidKind(kind string) bool { return kindRe.MatchString(kind) }

// bodyNameRe validates a named-secondary-body logical name (e.g. "outcome"). Same shape
// as a kind label so the derived sibling file name `<id>.<name>.md` stays a safe single
// path segment (no traversal, no separators) whatever kind attaches it.
var bodyNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidBodyName reports whether name is a legal named-secondary-body label.
func ValidBodyName(name string) bool { return bodyNameRe.MatchString(name) }

// ErrConflict is returned by Transition when the item changed since the caller last saw
// it (its status or updated_at no longer matches what the caller expected) — the
// optimistic-concurrency refusal. It is a LOUD, no-write outcome: last-writer-wins is
// never applied.
var ErrConflict = errors.New("routing: item changed since it was read (optimistic-concurrency refusal — re-read and retry)")

// Store is the routing item store rooted at Dir.
type Store struct{ Dir string }

// Open returns a Store rooted at dir (e.g. usage.DefaultRoutingDir, or a test's temp
// dir / a --routing-dir flag).
func Open(dir string) *Store { return &Store{Dir: dir} }

// NewID returns a sortable, collision-resistant item id: a compact RFC3339 UTC stamp
// (sorts by creation time) plus 6 random hex chars (disambiguates same-second creates).
// crypto/rand failure (effectively never) degrades to a time-nanosecond suffix rather
// than panicking — an id must always be mintable.
func NewID(now time.Time) string {
	stamp := now.UTC().Format("20060102T150405")
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%06x", stamp, now.UTC().Nanosecond()&0xffffff)
	}
	return stamp + "-" + hex.EncodeToString(b[:])
}

// itemDir is the directory an item of (kind, toSlug) lives in.
func (s *Store) itemDir(kind, toSlug string) string {
	return filepath.Join(s.Dir, kind, toSlug)
}

// jsonPath / bodyPath are an item's envelope and (optional) prose-body paths.
func (s *Store) jsonPath(kind, toSlug, id string) string {
	return filepath.Join(s.itemDir(kind, toSlug), id+".json")
}
func (s *Store) bodyPath(kind, toSlug, id string) string {
	return filepath.Join(s.itemDir(kind, toSlug), id+".md")
}

// namedBodyPath is a named secondary body's sibling path: <id>.<name>.md, next to the
// envelope and the primary body.
func (s *Store) namedBodyPath(kind, toSlug, id, name string) string {
	return filepath.Join(s.itemDir(kind, toSlug), id+"."+name+".md")
}

// CreateParams is the input to Create. ToSlug addressing (known project vs reserved
// sigil) is validated by the CALLER via ResolveRecipient before Create — the store
// itself is addressing-agnostic (it will happily file an item under any slug), so a kind
// CLI must gate on ResolveRecipient first. Now defaults to time.Now().UTC() when zero.
type CreateParams struct {
	Kind        string
	FromSlug    string
	FromSession string
	ToSlug      string
	Subject     string
	Status      string // the kind's initial status (opaque to the substrate)
	Body        []byte // optional prose body; when non-nil an <id>.md sibling is written
	Parent      string // optional id of the item this one derives from (see Envelope.Parent)
	Now         time.Time
}

// Create mints an item, writes its optional prose body, then writes the envelope
// atomically, and returns the stored Envelope. It validates the kind label (the only
// store-level invariant here) and records a "created" history event. The body is written
// BEFORE the envelope so a reader that sees the envelope's body_file always finds the
// body already present (never a dangling reference).
func (s *Store) Create(p CreateParams) (Envelope, error) {
	if !ValidKind(p.Kind) {
		return Envelope{}, fmt.Errorf("routing: invalid kind %q (want [a-z][a-z0-9-]*)", p.Kind)
	}
	if p.ToSlug == "" {
		return Envelope{}, errors.New("routing: create requires a to_slug")
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id := NewID(now)
	dir := s.itemDir(p.Kind, p.ToSlug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Envelope{}, fmt.Errorf("routing: mkdir item dir %s: %w", dir, err)
	}

	env := Envelope{
		ID:          id,
		Kind:        p.Kind,
		FromSlug:    p.FromSlug,
		FromSession: p.FromSession,
		ToSlug:      p.ToSlug,
		Subject:     p.Subject,
		Status:      p.Status,
		Parent:      p.Parent,
		CreatedAt:   now,
		UpdatedAt:   now,
		History: []Event{{
			TS: now, BySlug: p.FromSlug, BySession: p.FromSession,
			Action: "created", Note: p.Subject,
		}},
	}
	if p.Body != nil {
		env.BodyFile = id + ".md"
		if err := writeFileAtomic(s.bodyPath(p.Kind, p.ToSlug, id), p.Body); err != nil {
			return Envelope{}, err
		}
	}
	if err := s.writeEnvelope(env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// Read loads one item's envelope.
func (s *Store) Read(kind, toSlug, id string) (Envelope, error) {
	return readEnvelope(s.jsonPath(kind, toSlug, id))
}

// ReadBody returns the item's prose body, or (nil, nil) when it has none.
func (s *Store) ReadBody(env Envelope) ([]byte, error) {
	if env.BodyFile == "" {
		return nil, nil
	}
	return os.ReadFile(s.bodyPath(env.Kind, env.ToSlug, env.ID))
}

// ReadNamedBody returns a named secondary body's content (env.Bodies[name]), or
// (nil, nil) when the item carries no body under that name. Kind-agnostic: the caller
// names the logical body (e.g. "outcome"); the substrate never interprets it.
func (s *Store) ReadNamedBody(env Envelope, name string) ([]byte, error) {
	if env.Bodies[name] == "" {
		return nil, nil
	}
	return os.ReadFile(s.namedBodyPath(env.Kind, env.ToSlug, env.ID, name))
}

// ErrNotFound is returned by Locate when no item of the given kind carries the id — a
// clean, comparable sentinel so a kind CLI can distinguish "no such id" from a real
// read error.
var ErrNotFound = errors.New("routing: no item with that id")

// Locate finds one item by (kind, id) WITHOUT the caller knowing its to_slug — the item
// lives at <dir>/<kind>/<to-slug>/<id>.json, but a party that is NOT the recipient (a
// brief's sender running confirm/withdraw) doesn't know that path segment. It scans the
// kind's per-slug dirs for <id>.json. A missing store/kind dir is ErrNotFound (not a
// hard error). Kind-agnostic: it never inspects the envelope's contents.
func (s *Store) Locate(kind, id string) (Envelope, error) {
	if !ValidKind(kind) {
		return Envelope{}, fmt.Errorf("routing: invalid kind %q", kind)
	}
	kindDir := filepath.Join(s.Dir, kind)
	slugs, err := os.ReadDir(kindDir)
	if err != nil {
		if os.IsNotExist(err) {
			return Envelope{}, ErrNotFound
		}
		return Envelope{}, fmt.Errorf("routing: read kind dir %s: %w", kindDir, err)
	}
	for _, sl := range slugs {
		if !sl.IsDir() {
			continue
		}
		p := filepath.Join(kindDir, sl.Name(), id+".json")
		if _, statErr := os.Stat(p); statErr == nil {
			return readEnvelope(p)
		}
	}
	return Envelope{}, ErrNotFound
}

// TransitionParams drives a status transition with the optimistic-concurrency guard.
// ExpectedStatus MUST equal the item's current status; ExpectedUpdatedAt, when non-zero,
// must ALSO equal the current updated_at. Either mismatch is refused with ErrConflict
// and NO write — this is the cross-session analogue of the queue's single-writer rule,
// so two sessions racing one item can't silently clobber each other. Terminal marks the
// item closed (sets ClosedAt), which is the ONLY thing that makes it retention-eligible.
type TransitionParams struct {
	ExpectedStatus    string
	ExpectedUpdatedAt time.Time
	NewStatus         string
	Terminal          bool
	Action            string
	Note              string
	ActorSlug         string
	ActorSession      string
	Now               time.Time
	// AttachBodies are named secondary prose bodies to durably attach as part of THIS
	// transition (logical name -> content). Each is written to its <id>.<name>.md sibling
	// BEFORE the envelope is rewritten and recorded in env.Bodies, so a reader that sees
	// the recorded name always finds the file present (the same write-body-first ordering
	// Create uses). Kind-agnostic: the substrate stores them, never interprets them. An
	// invalid body name is refused (no partial write).
	AttachBodies map[string][]byte
}

// Transition re-reads the current envelope, enforces the optimistic-concurrency guard,
// appends a history event, sets the new status + updated_at (and closed_at when
// Terminal), and writes the whole envelope atomically. It does NOT enforce any
// kind-specific legality (that is the kind's CLI in build 2/3) — only the generic guard.
// On a guard mismatch it returns ErrConflict having written NOTHING.
func (s *Store) Transition(kind, toSlug, id string, p TransitionParams) (Envelope, error) {
	env, err := s.Read(kind, toSlug, id)
	if err != nil {
		return Envelope{}, err
	}
	// The optimistic-concurrency guard: refuse loudly if the item is not in the state
	// the caller last saw. This RE-READ-before-write is what makes concurrent sessions
	// safe without a lock — the loser of a race is refused, never silently overwritten.
	if env.Status != p.ExpectedStatus {
		return Envelope{}, fmt.Errorf("%w: expected status %q, found %q", ErrConflict, p.ExpectedStatus, env.Status)
	}
	if !p.ExpectedUpdatedAt.IsZero() && !env.UpdatedAt.Equal(p.ExpectedUpdatedAt) {
		return Envelope{}, fmt.Errorf("%w: expected updated_at %s, found %s", ErrConflict, p.ExpectedUpdatedAt.UTC(), env.UpdatedAt.UTC())
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Attach any named secondary bodies BEFORE rewriting the envelope, so the envelope
	// never references a body file that isn't on disk yet. Validate every name up front
	// (a bad name aborts with nothing written). The optimistic-concurrency guard above
	// has already passed, so writing the bodies now is safe against a raced flip.
	if len(p.AttachBodies) > 0 {
		for name := range p.AttachBodies {
			if !ValidBodyName(name) {
				return Envelope{}, fmt.Errorf("routing: invalid secondary-body name %q (want [a-z][a-z0-9-]*)", name)
			}
		}
		if env.Bodies == nil {
			env.Bodies = map[string]string{}
		}
		for name, data := range p.AttachBodies {
			if err := writeFileAtomic(s.namedBodyPath(env.Kind, env.ToSlug, env.ID, name), data); err != nil {
				return Envelope{}, err
			}
			env.Bodies[name] = env.ID + "." + name + ".md"
		}
	}
	env.Status = p.NewStatus
	env.UpdatedAt = now
	env.History = append(env.History, Event{
		TS: now, BySlug: p.ActorSlug, BySession: p.ActorSession,
		Action: p.Action, Note: p.Note,
	})
	if p.Terminal && env.ClosedAt == nil {
		closed := now
		env.ClosedAt = &closed
	}
	if err := s.writeEnvelope(env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// InboxFilter narrows an Inbox listing. Empty fields mean "no filter on this dimension".
type InboxFilter struct {
	Kind   string
	Status string
}

// Inbox returns every item addressed to toSlug (across all kinds), newest-first
// (CreatedAt descending, id as a stable tiebreak), optionally filtered by kind/status.
// A missing store dir is the ordinary "nothing yet" state → empty, nil. A malformed
// individual envelope is skipped (one bad file never blinds the whole read), same rule
// as the presence/project registries.
func (s *Store) Inbox(toSlug string, f InboxFilter) ([]Envelope, error) {
	items, err := s.scan(func(e Envelope) bool {
		if e.ToSlug != toSlug {
			return false
		}
		if f.Kind != "" && e.Kind != f.Kind {
			return false
		}
		if f.Status != "" && e.Status != f.Status {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID > items[j].ID
	})
	return items, nil
}

// List returns every item matching f across ALL recipients, oldest-updated first (the
// longest-parked first, which is the order an owner works a waiting queue in). It is the
// recipient-less sibling of Inbox: same filter, same skip-malformed rule, READ-ONLY. Like
// everything in this package it is kind-agnostic - the caller supplies the status string.
func (s *Store) List(f InboxFilter) ([]Envelope, error) {
	items, err := s.scan(func(e Envelope) bool {
		if f.Kind != "" && e.Kind != f.Kind {
			return false
		}
		if f.Status != "" && e.Status != f.Status {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
			return items[i].UpdatedAt.Before(items[j].UpdatedAt)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

// scan walks <dir>/<kind>/<to-slug>/*.json and returns every envelope keep() accepts.
// A missing root dir yields (nil, nil). Non-json files, sub-dirs at the wrong depth, and
// unparseable envelopes are skipped. Read failures on the ROOT dir surface as an error
// (a real "can't read the store" condition the caller decides on); a failure reading one
// nested dir/file is skipped, not fatal.
func (s *Store) scan(keep func(Envelope) bool) ([]Envelope, error) {
	kinds, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("routing: read store dir %s: %w", s.Dir, err)
	}
	var out []Envelope
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		slugs, err := os.ReadDir(filepath.Join(s.Dir, k.Name()))
		if err != nil {
			continue
		}
		for _, sl := range slugs {
			if !sl.IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(s.Dir, k.Name(), sl.Name()))
			if err != nil {
				continue
			}
			for _, fe := range files {
				if fe.IsDir() || filepath.Ext(fe.Name()) != ".json" {
					continue
				}
				env, err := readEnvelope(filepath.Join(s.Dir, k.Name(), sl.Name(), fe.Name()))
				if err != nil {
					continue
				}
				if keep(env) {
					out = append(out, env)
				}
			}
		}
	}
	return out, nil
}

// PruneCandidate is one item retention would remove: its identity + why (age past TTL).
type PruneCandidate struct {
	Env Envelope
	Age time.Duration
}

// PruneResult is Prune's outcome — the candidates it found, and (when apply) which were
// actually removed.
type PruneResult struct {
	Candidates []PruneCandidate
	Removed    []string // ids actually removed (only when apply)
	DryRun     bool
}

// Prune finds TERMINAL items whose ClosedAt is older than ttl and, when apply, removes
// them (envelope + prose body together). It NEVER considers a non-terminal item, at any
// age — a live hand-off is kept forever; only a closed item ages out. Dry-run (apply
// false) mutates nothing. Conservative by construction: the terminal gate is the ONLY
// thing that makes an item eligible, so a mis-set status can never delete live work.
func (s *Store) Prune(ttl time.Duration, now time.Time, apply bool) (PruneResult, error) {
	res := PruneResult{DryRun: !apply}
	items, err := s.scan(func(e Envelope) bool { return true })
	if err != nil {
		return res, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	for _, e := range items {
		if !e.Terminal() {
			continue // NEVER prune a non-terminal item — the load-bearing invariant.
		}
		age := now.Sub(*e.ClosedAt)
		if age <= ttl {
			continue
		}
		res.Candidates = append(res.Candidates, PruneCandidate{Env: e, Age: age})
		if apply {
			if e.BodyFile != "" {
				_ = os.Remove(s.bodyPath(e.Kind, e.ToSlug, e.ID))
			}
			for name := range e.Bodies {
				_ = os.Remove(s.namedBodyPath(e.Kind, e.ToSlug, e.ID, name))
			}
			if err := os.Remove(s.jsonPath(e.Kind, e.ToSlug, e.ID)); err == nil {
				res.Removed = append(res.Removed, e.ID)
			}
		}
	}
	return res, nil
}

// StaleNonTerminal returns every NON-terminal item whose updated_at is older than window
// — a dropped hand-off the recipient project hasn't acted on. Used by doctor's
// routing-inbox check. ok=false ONLY on a genuine store-read failure; a missing/absent
// store is ok=true, empty (fail-open — never a hard error).
func (s *Store) StaleNonTerminal(window time.Duration, now time.Time) (items []Envelope, ok bool) {
	cutoff := now.Add(-window)
	items, err := s.scan(func(e Envelope) bool {
		return !e.Terminal() && e.UpdatedAt.Before(cutoff)
	})
	if err != nil {
		return nil, false
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, true
}

// writeEnvelope marshals env and writes its <id>.json atomically.
func (s *Store) writeEnvelope(env Envelope) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("routing: marshal envelope: %w", err)
	}
	return writeFileAtomic(s.jsonPath(env.Kind, env.ToSlug, env.ID), data)
}

// readEnvelope loads and unmarshals one envelope file.
func readEnvelope(path string) (Envelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("routing: parse %s: %w", path, err)
	}
	return env, nil
}

// writeFileAtomic writes data to path via a same-dir temp file + os.Rename — the SAME
// temp+rename discipline internal/queue and the presence/project registries use, so a
// concurrent reader never sees a partial file and an interrupted write leaves the
// previous file (or nothing) intact, never a truncated one.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("routing: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".leadkit-routing-*.tmp")
	if err != nil {
		return fmt.Errorf("routing: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("routing: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("routing: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("routing: rename into place: %w", err)
	}
	return nil
}
