// Command leadkit is a local toolkit for the autonomous-lead workflow. Phase 1: the
// `burn` subcommand (the parallel-workstream governor). See leadkit-SPEC.md.
package main

import (
	"fmt"
	"os"
)

// Version is leadkit's own version — printed by `leadkit version`/`--version` and
// doctor's "binary" check. A release build stamps the real tag in with
// -ldflags "-X main.Version=..."; a local build keeps this -dev default.
// Declared as a VAR, not a const, because a const cannot be overridden that way.
var Version = "leadkit 0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: leadkit <burn|burn-report|serve|queue|status|rate|gate|review|pr|forge|publish|push|merge-msg|skip-ci|config|goal|state|hook|audit|lock|unlock|doctor|init|team|orchestrate|task|issue|hierarchy|get|list|tree|spike|decision|sync|test|onboard|milestone|routing|brief|register|who|registry|whoami|version> [flags]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "burn-report":
		runBurnReport(os.Args[2:])
	case "burn":
		runBurn(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "queue":
		runQueue(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "rate":
		runRate(os.Args[2:])
	case "gate":
		runGate(os.Args[2:])
	case "review":
		runReview(os.Args[2:])
	case "pr":
		runPR(os.Args[2:])
	case "forge":
		runForge(os.Args[2:])
	case "publish":
		runPublish(os.Args[2:])
	case "push":
		runPush(os.Args[2:])
	case "merge-msg":
		runMergeMsg(os.Args[2:])
	case "skip-ci":
		runSkipCI(os.Args[2:])
	case "config":
		runConfig(os.Args[2:])
	case "goal":
		runGoal(os.Args[2:])
	case "state":
		runState(os.Args[2:])
	case "hook":
		runHook(os.Args[2:])
	case "audit":
		runAudit(os.Args[2:])
	case "lock":
		runLock(os.Args[2:])
	case "unlock":
		runUnlock(os.Args[2:])
	case "doctor":
		runDoctor(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "team":
		runTeam(os.Args[2:])
	case "orchestrate":
		runOrchestrate(os.Args[2:])
	case "task":
		runTask(os.Args[2:])
	case "issue":
		runIssue(os.Args[2:])
	case "hierarchy":
		runHierarchy(os.Args[2:])
	case "get":
		runGet(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "tree":
		runTree(os.Args[2:])
	case "spike":
		runSpike(os.Args[2:])
	case "decision":
		runDecision(os.Args[2:])
	case "sync":
		runSync(os.Args[2:])
	case "test":
		runTestCmd(os.Args[2:])
	case "onboard":
		runOnboard(os.Args[2:])
	case "milestone":
		runMilestone(os.Args[2:])
	case "routing":
		runRouting(os.Args[2:])
	case "brief":
		runBrief(os.Args[2:])
	case "register":
		runRegister(os.Args[2:])
	case "who":
		runWho(os.Args[2:])
	case "registry":
		runRegistry(os.Args[2:])
	case "whoami":
		runWhoami(os.Args[2:])
	case "version", "--version":
		fmt.Println(Version)
	default:
		fmt.Fprintf(os.Stderr, "leadkit: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}
