package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultClaimTTL is how long a claim holds without being renewed. A claim exists so a second owner on
// the lane does not start the work a first one is already doing, and it has to lapse: an agent that
// dies holding one must not keep its ticket out of the lane. Two hours is longer than a review takes
// and shorter than a shift; a longer job renews with `queue claim` on the same entry.
const DefaultClaimTTL = 2 * time.Hour

// claimsDir holds one sidecar per claimed in-progress ticket. It is a dot directory, so every lister in
// this package (which skips dot names) leaves it alone, and it sits beside the entries it describes so a
// claim cannot outlive the lane it was made in.
const claimsDir = ".claims"

// Claim is who holds an in-progress ticket, since when, and for how long. By is the name the claimant
// asserts; nothing authenticates it (ticket 205), so a claim stops an honest peer from duplicating work,
// not a hostile one. The lifetime is the claim's own, written by the claimant, so every reader agrees on
// when it lapses whatever it would have chosen itself.
type Claim struct {
	By         string    `json:"by"`
	At         time.Time `json:"at"`
	TTLSeconds int       `json:"ttl_seconds,omitempty"`
}

func (c Claim) ttl() time.Duration {
	if c.TTLSeconds <= 0 {
		return DefaultClaimTTL
	}
	return time.Duration(c.TTLSeconds) * time.Second
}

// Live reports whether the claim still holds at now.
func (c Claim) Live(now time.Time) bool {
	return now.Before(c.At.Add(c.ttl()))
}

func (c Claim) String() string {
	return fmt.Sprintf("claimed by %q at %s, expires %s", c.By, c.At.UTC().Format(time.RFC3339), c.At.Add(c.ttl()).UTC().Format(time.RFC3339))
}

// claimFile is the sidecar path for the queue entry. It is derived from the entry's own directory, so it
// is only meaningful when that directory is an in-progress state.
func claimFile(entry string) string {
	return filepath.Join(filepath.Dir(entry), claimsDir, filepath.Base(entry)+".json")
}

func claimLockFile(entry string) string {
	return filepath.Join(filepath.Dir(entry), claimsDir, filepath.Base(entry)+".lock")
}

func inProgressDir(entry string) bool {
	return filepath.Base(filepath.Dir(entry)) == "in-progress"
}

// ReadClaim returns the claim on an in-progress entry. ok is false for an entry outside in-progress, an
// entry never claimed, and a sidecar that cannot be read: an unreadable claim is no claim, because the
// alternative is a ticket nobody can take.
func ReadClaim(entry string) (Claim, bool) {
	if !inProgressDir(entry) {
		return Claim{}, false
	}
	data, err := os.ReadFile(claimFile(entry))
	if err != nil {
		return Claim{}, false
	}
	var c Claim
	if json.Unmarshal(data, &c) != nil || c.By == "" || c.At.IsZero() {
		return Claim{}, false
	}
	return c, true
}

// ClaimedByOther returns the live claim on entry when its holder is not by. An empty by is nobody, so any
// live claim is another's; callers that do not know who they are use that to ask "is this held at all".
func ClaimedByOther(entry, by string, now time.Time) (Claim, bool) {
	c, ok := ReadClaim(entry)
	if !ok || !c.Live(now) || c.By == by {
		return Claim{}, false
	}
	return c, true
}

// clearClaim removes entry's sidecar and its lock file. A missing one is the normal case.
func clearClaim(entry string) {
	_ = os.Remove(claimFile(entry))
	_ = os.Remove(claimLockFile(entry))
}

func validClaimant(by string) error {
	if strings.TrimSpace(by) == "" || strings.ContainsAny(by, "/\\\x00\n\r") {
		return fmt.Errorf("refuse — a claim needs a claimant name (-by), without path separators or newlines; got %q", by)
	}
	return nil
}

// writeClaim replaces entry's claim. Callers hold the claim lock.
func writeClaim(entry string, c Claim) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := claimFile(entry)
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ClaimEntry makes by the holder of a ticket. from is a pending entry, which is moved into its lane's
// in-progress (every refusal Move makes applies, including a blocked ticket) and claimed, or an
// in-progress entry, which is renewed when by already holds it, taken over when the holder's claim has
// lapsed, and adopted when it never had one. A live claim by someone else is refused, naming who holds
// it and until when. It returns the claim written.
//
// Reading the holder and writing the new claim happen under one lock, so of any number of owners racing
// for the same ticket (a fresh one, an unclaimed one, one whose holder died) exactly one is told it won.
func ClaimEntry(root, from, by string, now time.Time, ttl time.Duration) (Claim, error) {
	if err := validClaimant(by); err != nil {
		return Claim{}, err
	}
	if _, err := os.Lstat(from); err != nil {
		return Claim{}, fmt.Errorf("refuse — source does not exist: %s", from)
	}
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	claim := Claim{By: by, At: now.UTC().Truncate(time.Second), TTLSeconds: int(ttl / time.Second)}

	switch filepath.Base(filepath.Dir(from)) {
	case "pending":
		dest := filepath.Join(filepath.Dir(filepath.Dir(from)), "in-progress") + string(os.PathSeparator)
		// The rename inside Move is atomic, so of owners racing for a pending entry one moves it and the
		// rest find it gone. Only the winner reaches the write below.
		if err := Move(root, from, dest, false); err != nil {
			return Claim{}, err
		}
		entry := filepath.Join(dest, filepath.Base(from))
		err := withClaimLock(entry, func() error { return writeClaim(entry, claim) })
		if err != nil {
			return Claim{}, fmt.Errorf("the ticket was moved to %s but its claim could not be recorded (%v): re-run `queue claim` on it there", entry, err)
		}
		return claim, nil
	case "in-progress":
		err := withClaimLock(from, func() error {
			if held, ok := ClaimedByOther(from, by, now); ok {
				return fmt.Errorf("refuse — %s is already %s. Another owner is on it: take a different ticket, or wait for the claim to lapse", filepath.Base(from), held.String())
			}
			return writeClaim(from, claim)
		})
		if err != nil {
			return Claim{}, err
		}
		return claim, nil
	default:
		return Claim{}, fmt.Errorf("refuse — %s is in %q: only a pending or in-progress ticket can be claimed", filepath.Base(from), filepath.Base(filepath.Dir(from)))
	}
}
