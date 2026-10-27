package usage

// The account-level ROUTING store's default directory (docs/build-tasks/
// routing-substrate.md). Co-located here with DefaultPresenceDir/DefaultProjectsDir
// because it is the same class of account-wide leadkit state — one known place, keyed
// by project slug, that multiple sessions read and write. The routing SUBSTRATE itself
// lives in internal/routing (envelope, atomic + optimistic-concurrency writes, inbox,
// delivery hinting, retention); only the default path constant lives here, so a caller
// wiring routing reads its dir the same way it reads the presence/project registries'.

import (
	"os"
	"path/filepath"
)

// DefaultRoutingDir is the account-wide directory the routing store's
// <kind>/<to-slug>/<id>.json envelopes live under. Overridable per call (the store
// takes an explicit dir) and, for the CLI/hook, via $LEADKIT_ROUTING_DIR.
var DefaultRoutingDir = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "routing")
