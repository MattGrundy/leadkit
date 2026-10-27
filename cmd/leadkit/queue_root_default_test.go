package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// queue move, watch and sweep default -queue to the same root as gate/review/task/pr:
// the profile's queue_root, with $LEADKIT_QUEUE overriding it. watch and sweep used a
// bare queue.DefaultRoot that ignored the profile. -h prints each flag default.
func TestQueueWatchSweepMove_DefaultRootMatchesProfileAndEnv(t *testing.T) {
	home, repo := cfgPathRepo(t)
	profile := filepath.Join(repo, "leadkit.yaml")
	writeFile(t, profile, "queue_root: my-queue\n")
	wantProfile := filepath.Join(repo, "my-queue")
	envRoot := filepath.Join(t.TempDir(), "env-queue")

	for _, sub := range []string{"move", "watch", "sweep"} {
		out, _ := runIn(t, repo, home, []string{"LEADKIT_PROFILE=" + profile, "LEADKIT_QUEUE="}, "queue", sub, "-h")
		if !strings.Contains(out, `(default "`+wantProfile+`")`) {
			t.Errorf("queue %s default root: want profile queue_root %q in help, got:\n%s", sub, wantProfile, out)
		}
		out, _ = runIn(t, repo, home, []string{"LEADKIT_PROFILE=" + profile, "LEADKIT_QUEUE=" + envRoot}, "queue", sub, "-h")
		if !strings.Contains(out, `(default "`+envRoot+`")`) {
			t.Errorf("queue %s default root: want $LEADKIT_QUEUE %q in help, got:\n%s", sub, envRoot, out)
		}
	}
}

func TestQueueMoveHelp_DocumentsCwdRelativeArgs(t *testing.T) {
	home, repo := cfgPathRepo(t)
	out, _ := runIn(t, repo, home, nil, "queue", "move", "-h")
	if !strings.Contains(out, "relative to the current directory") {
		t.Errorf("queue move -h must say <from>/<to> are cwd-relative, got:\n%s", out)
	}
}
