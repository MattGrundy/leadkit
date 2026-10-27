package queue

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// wakerLogRelPath is $HOME-relative: Watch's log lines are tee'd to stdout and here,
// mirroring queue-waker.sh's log file.
const wakerLogRelPath = ".claude/queue-waker.log"

// pendingDirName is the status directory Watch reacts to arrivals in.
const pendingDirName = "pending"

// Watch is the queue-waker.sh equivalent: a recursive filesystem watch over root that
// LOGS an agent's pending/ inbox arrivals (a per-project "who has pending work" signal)
// and, only if wakeCommand is set, execs it to notify the agent out-of-band. fsnotify is
// not recursive, so every existing directory under root is added up front, and newly
// created directories are added as they appear. wakeCommand is the OPTIONAL configured
// notifier (empty = the default: exec nothing — the agent's own event-driven `queue wait`
// is the wake path). dryRun prints what would be sent instead of exec'ing. Blocks until
// the watcher's channels close (e.g. Watcher.Close on process signal) or a setup error
// occurs.
func Watch(root, wakeCommand string, dryRun bool) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("queue-watch: %w", err)
	}
	defer w.Close()

	logFile, ferr := openWakerLog()
	if ferr != nil {
		// Best-effort logging (mirrors the shell's tee): a log write failure
		// shouldn't stop the watcher, stdout still gets every line.
		fmt.Fprintf(os.Stderr, "queue-watch: could not open log file: %v\n", ferr)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	log := func(format string, a ...any) {
		line := fmt.Sprintf("%s %s", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, a...))
		fmt.Println(line)
		if logFile != nil {
			fmt.Fprintln(logFile, line)
		}
	}

	if err := addTreeRecursive(w, root); err != nil {
		return fmt.Errorf("queue-watch: %w", err)
	}
	log("watching %s", root)

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			handleWatchEvent(w, root, ev, wakeCommand, dryRun, log)
		case werr, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log("watcher error: %v", werr)
		}
	}
}

// addTreeRecursive adds path, and every directory beneath it, to w — the walk that
// stands in for fsnotify's missing recursive-watch support.
func addTreeRecursive(w *fsnotify.Watcher, path string) error {
	return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return w.Add(p)
		}
		return nil
	})
}

// handleWatchEvent reacts to one fsnotify event: a newly created directory is added
// to the watch (so new agent/status dirs get seen), and an event on a pending/*.md
// entry is run through the wake check.
func handleWatchEvent(w *fsnotify.Watcher, root string, ev fsnotify.Event, wakeCommand string, dryRun bool, log func(string, ...any)) {
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			if err := addTreeRecursive(w, ev.Name); err != nil {
				log("could not watch new dir %s: %v", ev.Name, err)
			}
			return
		}
	}

	agent, ok := matchPendingEntry(root, ev.Name)
	if !ok {
		return
	}
	maybeWake(root, agent, wakeCommand, dryRun, log)
}

// matchPendingEntry reports whether path is a "*/pending/*.md" entry under root and,
// if so, the owning agent — the path's first segment after root. Agents whose name
// starts with "_" (scratch/test fixtures) never match.
func matchPendingEntry(root, path string) (agent string, ok bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 3 || parts[1] != pendingDirName || !strings.HasSuffix(parts[2], ".md") {
		return "", false
	}
	if strings.HasPrefix(parts[0], "_") {
		return "", false
	}
	return parts[0], true
}

// maybeWake reacts to agent's pending/ inbox once it is confirmed non-empty. fsnotify
// also fires on the REMOVAL when a reviewer claims a ticket (relinking it out of
// pending/); re-checking the inbox here, rather than trusting the triggering event,
// is what stops that from looping — waking an agent for taking its own work.
//
// The waker ALWAYS logs the arrival (a per-project "who has pending work" observability
// signal), then wakes via the OPTIONAL configured wakeCommand. If wakeCommand is EMPTY
// (the default, and the correct default now): it execs NOTHING and logs that the agent's
// own event-driven `queue wait` (fsnotify) will pick the ticket up — it does NOT log a
// wake-command-failed line, because there is no missing binary to fail on. If wakeCommand
// is SET: it's exec'd with {agent}/{msg} placeholders substituted, and a failure is logged
// (ticket left in place) — now an opt-in, never the failing default.
func maybeWake(root, agent, wakeCommand string, dryRun bool, log func(string, ...any)) {
	pendDir := filepath.Join(root, agent, pendingDirName)
	entries := listTickets(pendDir)
	n := len(entries)
	if n == 0 {
		return
	}

	ticketPath := fmt.Sprintf("queue/%s/pending/%s", agent, entries[0])
	msg := fmt.Sprintf(
		"Gate ticket in your inbox: %s (%d pending). Claim it and record with ~/.claude/gate-verdict.sh --ticket %s --verdict ready|changes-needed --by %s",
		ticketPath, n, ticketPath, agent,
	)

	if dryRun {
		log("WOULD wake %s: %s", agent, msg)
		return
	}

	log("waking %s: %s", agent, msg)

	if wakeCommand == "" {
		// No external notifier configured — the agent's event-driven `queue wait`
		// wakes itself the instant the ticket lands (internal/queue.Wait's fsnotify).
		// Log the arrival and move on: nothing to exec, nothing to fail.
		log("pending ticket for %s (%d); its `queue wait` will pick it up", agent, n)
		return
	}

	args := substituteWakeArgs(wakeCommand, agent, msg)
	if len(args) == 0 {
		return
	}
	if err := exec.Command(args[0], args[1:]...).Run(); err != nil {
		log("wake_command %q failed for %s, ticket left in place: %v", wakeCommand, agent, err)
	}
}

// substituteWakeArgs splits wakeCommand into fields and substitutes the {agent}/{msg}
// placeholders in each — so "claude-wake {agent} {msg}" reproduces the historical
// exec.Command("claude-wake", agent, msg) invocation exactly, and {msg} (which contains
// spaces) becomes a single argument.
func substituteWakeArgs(wakeCommand, agent, msg string) []string {
	fields := strings.Fields(wakeCommand)
	args := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, "{agent}", agent)
		f = strings.ReplaceAll(f, "{msg}", msg)
		args = append(args, f)
	}
	return args
}

// openWakerLog opens (creating if needed) $HOME/.claude/queue-waker.log for append.
func openWakerLog() (*os.File, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return nil, fmt.Errorf("HOME not set")
	}
	path := filepath.Join(home, wakerLogRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}
