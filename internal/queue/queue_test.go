package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// lchtimesForTest sets a symlink's own mtime to an explicit time without following it
// — like chtimesNoDeref, but with a caller-chosen time so tests can plant an "old"
// mtime and then assert Sweep's poke bumped it.
func lchtimesForTest(path string, t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Lutimes(path, []unix.Timeval{tv, tv})
}

// setupSymlinkEntry creates a real file `name` under store and a symlink to it at
// linkPath (creating linkPath's parent dirs as needed), returning the target's path.
func setupSymlinkEntry(t *testing.T, store, linkPath, name string) string {
	t.Helper()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(store, name)
	if err := os.WriteFile(target, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestMove_DryRunMutatesNothing(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "agentA", "pending", "ticket.md")
	to := filepath.Join(root, "agentA", "in-progress", "ticket.md")
	target := setupSymlinkEntry(t, store, from, "ticket.md")

	if err := Move(root, from, to, true); err != nil {
		t.Fatalf("Move (dry-run): %v", err)
	}
	if _, err := os.Lstat(from); err != nil {
		t.Errorf("dry-run must leave the source in place: %v", err)
	}
	if _, err := os.Lstat(to); err == nil {
		t.Error("dry-run must not create the destination")
	}
	got, err := os.Readlink(from)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("dry-run must not touch the source symlink's target: got %q, want %q", got, target)
	}
}

func TestMove_RelinksSymlinkWithoutDereferencing(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "agentA", "pending", "ticket.md")
	to := filepath.Join(root, "agentA", "in-progress", "ticket.md")
	target := setupSymlinkEntry(t, store, from, "ticket.md")

	if err := Move(root, from, to, false); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := os.Lstat(from); err == nil {
		t.Errorf("source still exists at %s", from)
	}

	fi, err := os.Lstat(to)
	if err != nil {
		t.Fatalf("dest missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("moved entry is not a symlink (was dereferenced during the move)")
	}
	got, err := os.Readlink(to)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if got != target {
		t.Errorf("symlink target = %s, want unchanged %s", got, target)
	}
}

func TestMove_Refusals(t *testing.T) {
	tests := map[string]func(t *testing.T) (root, from, to string){
		"source outside root": func(t *testing.T) (string, string, string) {
			root := t.TempDir()
			outside := t.TempDir()
			from := filepath.Join(outside, "ticket.md")
			if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return root, from, filepath.Join(root, "agentA", "in-progress", "ticket.md")
		},
		"dest outside root": func(t *testing.T) (string, string, string) {
			root := t.TempDir()
			outside := t.TempDir()
			store := filepath.Join(root, "tasks")
			from := filepath.Join(root, "agentA", "pending", "ticket.md")
			setupSymlinkEntry(t, store, from, "ticket.md")
			return root, from, filepath.Join(outside, "ticket.md")
		},
		"nonexistent source": func(t *testing.T) (string, string, string) {
			root := t.TempDir()
			from := filepath.Join(root, "agentA", "pending", "missing.md")
			to := filepath.Join(root, "agentA", "in-progress", "missing.md")
			return root, from, to
		},
	}

	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root, from, to := setup(t)
			if err := Move(root, from, to, false); err == nil {
				t.Fatalf("Move(%s, %s): want error, got nil", from, to)
			}
		})
	}
}

// TestMove_DirDestTrailingSlashAppendsBasename is the #123 bug: a dest ending in "/"
// (an empty dir) must be treated as a destination DIRECTORY — Move appends the src
// basename — instead of erroring "file exists" against the dir. Fails before the fix,
// passes after.
func TestMove_DirDestTrailingSlashAppendsBasename(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "qa", "in-progress", "1454-x.md")
	setupSymlinkEntry(t, store, from, "1454-x.md")
	// An EMPTY dest dir, addressed with a trailing slash.
	destDir := filepath.Join(root, "qa", "pending")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	to := destDir + string(os.PathSeparator)

	if err := Move(root, from, to, false); err != nil {
		t.Fatalf("Move(dir-dest): %v", err)
	}
	got := filepath.Join(destDir, "1454-x.md")
	if fi, err := os.Lstat(got); err != nil {
		t.Fatalf("dest entry missing at %s: %v", got, err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("moved entry is not a symlink (dereferenced during the dir-dest move)")
	}
	if _, err := os.Lstat(from); err == nil {
		t.Errorf("source still exists at %s", from)
	}
}

// TestMove_DirDestExistingDirAppendsBasename: a dest that already exists as a
// directory (no trailing slash) is likewise treated as a destination dir.
func TestMove_DirDestExistingDirAppendsBasename(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "qa", "in-progress", "1454-x.md")
	setupSymlinkEntry(t, store, from, "1454-x.md")
	destDir := filepath.Join(root, "qa", "pending")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No trailing slash — but destDir exists as a directory.
	if err := Move(root, from, destDir, false); err != nil {
		t.Fatalf("Move(existing-dir dest): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destDir, "1454-x.md")); err != nil {
		t.Fatalf("dest entry missing at %s: %v", filepath.Join(destDir, "1454-x.md"), err)
	}
}

// TestMove_PlainDestKeepsExactName is the no-regression guard: a plain (non-dir,
// non-trailing-slash) dest keeps today's exact-name behaviour — the entry lands AT
// the given path, not under it.
func TestMove_PlainDestKeepsExactName(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "agentA", "pending", "ticket.md")
	setupSymlinkEntry(t, store, from, "ticket.md")
	to := filepath.Join(root, "agentA", "in-progress", "renamed.md")

	if err := Move(root, from, to, false); err != nil {
		t.Fatalf("Move(plain dest): %v", err)
	}
	if _, err := os.Lstat(to); err != nil {
		t.Fatalf("dest missing at exact path %s: %v", to, err)
	}
	// It must NOT have been treated as a dir (no nested basename dir created).
	if _, err := os.Lstat(filepath.Join(to, "ticket.md")); err == nil {
		t.Errorf("plain dest was wrongly treated as a directory")
	}
}

// TestMove_DirDestDryRunShowsResolvedDest: dry-run mutates nothing but resolves the
// dir-dest to the appended-basename path (a caller-visible sanity check).
func TestMove_DirDestDryRunShowsResolvedDest(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "qa", "in-progress", "1454-x.md")
	setupSymlinkEntry(t, store, from, "1454-x.md")
	destDir := filepath.Join(root, "qa", "pending")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Move(root, from, destDir+string(os.PathSeparator), true); err != nil {
		t.Fatalf("Move(dir-dest dry-run): %v", err)
	}
	// Nothing moved.
	if _, err := os.Lstat(from); err != nil {
		t.Errorf("dry-run must leave the source in place: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destDir, "1454-x.md")); err == nil {
		t.Errorf("dry-run must not create the resolved dest")
	}
}

// TestMove_DirDestRefusesClobberingRealFile: when the resolved dir-dest already
// exists as a real file, Move refuses rather than clobbering it.
func TestMove_DirDestRefusesClobberingRealFile(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	from := filepath.Join(root, "qa", "in-progress", "1454-x.md")
	setupSymlinkEntry(t, store, from, "1454-x.md")
	destDir := filepath.Join(root, "qa", "pending")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A REAL file already occupies the resolved dest name.
	if err := os.WriteFile(filepath.Join(destDir, "1454-x.md"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Move(root, from, destDir+string(os.PathSeparator), false); err == nil {
		t.Fatalf("Move(dir-dest onto existing real file): want refusal, got nil")
	}
	// The source must be untouched (refusal happens before any rename).
	if _, err := os.Lstat(from); err != nil {
		t.Errorf("refused move must leave the source in place: %v", err)
	}
}

func TestSweep(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")

	// agentA: one resolvable pending ticket, nothing in-progress -> STALLED.
	setupSymlinkEntry(t, store, filepath.Join(root, "agentA", "pending", "t1.md"), "t1.md")
	mustMkdirAll(t, filepath.Join(root, "agentA", "in-progress"))

	// agentB: a dangling symlink (target since removed) -> BROKEN, and since it's
	// the only pending entry (uncounted as "pend"), agentB is not also STALLED.
	setupSymlinkEntry(t, store, filepath.Join(root, "agentB", "pending", "t2.md"), "t2.md")
	if err := os.Remove(filepath.Join(store, "t2.md")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(root, "agentB", "in-progress"))

	// agentC: pending/ itself is a symlink (alias inbox) -> skipped entirely, so its
	// aliased ticket isn't double-counted against agentA.
	mustMkdirAll(t, filepath.Join(root, "agentC"))
	if err := os.Symlink(filepath.Join(root, "agentA", "pending"), filepath.Join(root, "agentC", "pending")); err != nil {
		t.Fatal(err)
	}

	// ROLES is a reserved dir name, always skipped even though it looks like an agent.
	setupSymlinkEntry(t, store, filepath.Join(root, "ROLES", "pending", "t3.md"), "t3.md")

	res, err := Sweep(root, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1 (lines: %v)", res.Stalled, res.Lines)
	}
	if res.Broken != 1 {
		t.Errorf("Broken = %d, want 1 (lines: %v)", res.Broken, res.Lines)
	}
	// The structured findings (feeding the artifact) must mirror the counts.
	if len(res.StalledLanes) != 1 || res.StalledLanes[0].Lane != "agentA" || res.StalledLanes[0].Pending != 1 {
		t.Errorf("StalledLanes = %+v, want one agentA lane with 1 pending", res.StalledLanes)
	}
	if len(res.BrokenLanes) != 1 || res.BrokenLanes[0].Lane != "agentB" {
		t.Errorf("BrokenLanes = %+v, want one agentB lane", res.BrokenLanes)
	}
}

// TestSweep_RecordsInProgressClaimMtimes asserts the frozen-claim census: Sweep records
// each in-progress ticket with the claim age taken from the SYMLINK's OWN mtime (Lstat,
// not dereferenced), so the frozen-claim detector reuses it instead of re-walking.
func TestSweep_RecordsInProgressClaimMtimes(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")

	link := filepath.Join(root, "devops", "in-progress", "t42.md")
	setupSymlinkEntry(t, store, link, "t42.md")
	claimed := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	if err := lchtimesForTest(link, claimed); err != nil {
		t.Fatal(err)
	}

	res, err := Sweep(root, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.InProgressClaims) != 1 {
		t.Fatalf("InProgressClaims = %+v, want exactly one", res.InProgressClaims)
	}
	c := res.InProgressClaims[0]
	if c.Lane != "devops" || c.Ticket != "t42.md" {
		t.Errorf("claim = %+v, want devops/t42.md", c)
	}
	if !c.ClaimedOK {
		t.Error("ClaimedOK should be true for a readable symlink mtime")
	}
	if !c.ClaimedAt.Equal(claimed) {
		t.Errorf("ClaimedAt = %v, want the symlink's own mtime %v (Lstat, not dereferenced)", c.ClaimedAt, claimed)
	}
}

func TestSweep_PokeBumpsStalledEntryMtimeWithoutDereferencing(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	link := filepath.Join(root, "agentA", "pending", "t1.md")
	target := setupSymlinkEntry(t, store, link, "t1.md")
	mustMkdirAll(t, filepath.Join(root, "agentA", "in-progress"))

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	if err := lchtimesForTest(link, old); err != nil {
		t.Fatal(err)
	}

	if _, err := Sweep(root, true); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	linkFi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !linkFi.ModTime().After(old) {
		t.Errorf("poked symlink mtime = %v, want bumped past %v", linkFi.ModTime(), old)
	}
	targetFi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !targetFi.ModTime().Equal(old) {
		t.Errorf("poke dereferenced the symlink: target mtime = %v, want unchanged %v", targetFi.ModTime(), old)
	}
}

func TestWait(t *testing.T) {
	t.Run("pending present returns WORK-PENDING", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "pending"))
		mustWriteFile(t, filepath.Join(dir, "pending", "t1.md"))

		res, err := Wait(dir, time.Second, 10*time.Millisecond, "")
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if res.Kind != "WORK-PENDING" {
			t.Errorf("Kind = %s, want WORK-PENDING", res.Kind)
		}
	})

	t.Run("in-progress takes precedence over pending", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "pending"))
		mustWriteFile(t, filepath.Join(dir, "pending", "t1.md"))
		mustMkdirAll(t, filepath.Join(dir, "in-progress"))
		mustWriteFile(t, filepath.Join(dir, "in-progress", "t2.md"))

		res, err := Wait(dir, time.Second, 10*time.Millisecond, "")
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if res.Kind != "ORPHAN-INPROGRESS" {
			t.Errorf("Kind = %s, want ORPHAN-INPROGRESS (in-progress must be drained first)", res.Kind)
		}
	})

	t.Run("empty dir times out", func(t *testing.T) {
		dir := t.TempDir()
		res, err := Wait(dir, 30*time.Millisecond, 10*time.Millisecond, "")
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if res.Kind != "TIMEOUT" {
			t.Errorf("Kind = %s, want TIMEOUT", res.Kind)
		}
	})

	t.Run("lock present returns LOCKED before checking the inbox at all", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "pending"))
		mustWriteFile(t, filepath.Join(dir, "pending", "t1.md")) // real work waiting — must still be ignored

		lockPath := filepath.Join(t.TempDir(), "PAUSE")
		mustWriteFile(t, lockPath)

		res, err := Wait(dir, time.Second, 10*time.Millisecond, lockPath)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if res.Kind != "LOCKED" {
			t.Errorf("Kind = %s, want LOCKED (checked ahead of pending work)", res.Kind)
		}
	})

	t.Run("lock absent behaves exactly like lockPath disabled", func(t *testing.T) {
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "pending"))
		mustWriteFile(t, filepath.Join(dir, "pending", "t1.md"))

		lockPath := filepath.Join(t.TempDir(), "PAUSE") // never created

		res, err := Wait(dir, time.Second, 10*time.Millisecond, lockPath)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if res.Kind != "WORK-PENDING" {
			t.Errorf("Kind = %s, want WORK-PENDING", res.Kind)
		}
	})
}

func TestWait_WakesOnArrival(t *testing.T) {
	cases := []struct {
		name   string
		lock   bool
		arrive func(t *testing.T, dir, lockPath string)
		want   string
	}{
		{
			name:   "pending arrival wakes the waiter",
			arrive: func(t *testing.T, dir, _ string) { mustWriteFile(t, filepath.Join(dir, "pending", "t1.md")) },
			want:   "WORK-PENDING",
		},
		{
			name:   "in-progress arrival wakes the waiter",
			arrive: func(t *testing.T, dir, _ string) { mustWriteFile(t, filepath.Join(dir, "in-progress", "t2.md")) },
			want:   "ORPHAN-INPROGRESS",
		},
		{
			name:   "lock arrival wakes the waiter",
			lock:   true,
			arrive: func(t *testing.T, _, lockPath string) { mustWriteFile(t, lockPath) },
			want:   "LOCKED",
		},
	}

	type result struct {
		res WaitResult
		err error
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustMkdirAll(t, filepath.Join(dir, "pending"))
			mustMkdirAll(t, filepath.Join(dir, "in-progress"))
			lockPath := ""
			if tc.lock {
				lockPath = filepath.Join(t.TempDir(), "PAUSE")
			}

			done := make(chan result, 1)
			// interval (20s) set FAR larger than the test's patience window so a pass
			// proves the fsnotify wake fired, not the fallback poll tick; timeout (30s)
			// generous so the wait can't spuriously TIMEOUT.
			go func() {
				res, err := Wait(dir, 30*time.Second, 20*time.Second, lockPath)
				done <- result{res, err}
			}()

			// Let the waiter reach its blocking fsnotify select before the arrival.
			time.Sleep(150 * time.Millisecond)
			tc.arrive(t, dir, lockPath)

			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("Wait: %v", got.err)
				}
				if got.res.Kind != tc.want {
					t.Errorf("Kind = %s, want %s", got.res.Kind, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("Wait did not return within 5s of the arrival — the event-driven wake did not fire (the poll interval is 20s, so only fsnotify could return this fast)")
			}
		})
	}
}

func TestWaitPoll_FallbackFindsWorkAndTimesOut(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "pending"))
	check := func() (WaitResult, bool) {
		if p := listEntries(filepath.Join(dir, "pending")); len(p) > 0 {
			return WaitResult{Kind: "WORK-PENDING", Msg: strings.Join(p, " ")}, true
		}
		return WaitResult{}, false
	}

	// Nothing present -> the fallback poll must still honour the deadline and TIMEOUT.
	res := waitPoll(check, time.Now().Add(30*time.Millisecond), 10*time.Millisecond, 30*time.Millisecond)
	if res.Kind != "TIMEOUT" {
		t.Errorf("Kind = %s, want TIMEOUT from the poll fallback", res.Kind)
	}

	// Work present -> the fallback poll must find it (the fsnotify-setup-failure path).
	mustWriteFile(t, filepath.Join(dir, "pending", "t1.md"))
	res = waitPoll(check, time.Now().Add(time.Second), 10*time.Millisecond, time.Second)
	if res.Kind != "WORK-PENDING" {
		t.Errorf("Kind = %s, want WORK-PENDING (poll fallback must still find work)", res.Kind)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDefaultRoot_EnvOverridesCwdRelative proves the queue-root default is
// project-agnostic (docs/build-tasks/project-agnostic-defaults.md §2): $LEADKIT_QUEUE
// wins when set, and with it unset the default is the cwd-relative ./queue — never a
// baked-in adopter path.
func TestDefaultRoot_EnvOverridesCwdRelative(t *testing.T) {
	t.Run("LEADKIT_QUEUE honored", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "custom", "queue")
		t.Setenv("LEADKIT_QUEUE", want)
		if got := DefaultRoot(); got != want {
			t.Errorf("DefaultRoot() = %q, want the $LEADKIT_QUEUE value %q", got, want)
		}
	})

	t.Run("unset falls back to cwd-relative ./queue", func(t *testing.T) {
		t.Setenv("LEADKIT_QUEUE", "")
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		got := DefaultRoot()
		want := filepath.Join(wd, "queue")
		if got != want {
			t.Errorf("DefaultRoot() = %q, want the cwd-relative %q", got, want)
		}
		// And it must NEVER be the old hardcoded adopter tree (the runtime value is
		// legitimately cwd-dependent, so we only assert the removed default's fragments
		// are gone — not the machine's own home path, which is naturally in the cwd).
		// The real removed fragment, not a stand-in: DefaultRoot() never contained a
		// generic name, so a renamed needle would guard nothing.
		for _, bad := range scrubterms.Optional(t).ForTests() {
			if strings.Contains(strings.ToLower(got), bad) {
				t.Errorf("DefaultRoot() = %q must not contain the baked-in fragment %q", got, bad)
			}
		}
	})
}
