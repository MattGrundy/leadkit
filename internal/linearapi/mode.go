// Package linearapi is leadkit's DIRECT Linear execution path: a small GraphQL
// client plus the mode/credential resolution that decides whether `sync` runs
// through the sync-executor agent's MCP (the default) or straight against the
// Linear API. It is the one deliberate, scoped exception to leadkit's "holds no
// Linear token" principle (docs/HIERARCHY-AND-SYNC.md): the key is read at
// runtime, used for the request's Authorization header, and never logged,
// echoed, or written anywhere.
package linearapi

import (
	"fmt"
	"strings"
)

// Execution modes.
const (
	ModeMCP    = "mcp"
	ModeDirect = "direct"
)

// EnvKey is the env var that carries the Linear API key — its mere presence is
// what auto-selects direct mode.
const EnvKey = "LINEAR_API_KEY"

// KeychainAccount is the keychain entry (account name, under the configured
// keychain_service) holding the key when it isn't in the env.
const KeychainAccount = "linear"

// ResolveMode picks the execution mode. Precedence: explicit flag > config
// (linear.sync_mode) > env-key default (a non-blank LINEAR_API_KEY means direct,
// else mcp). flag/cfg may be "" (unset). Only the env's PRESENCE is consulted
// here — the key value itself never enters this function. The keychain never
// auto-selects direct.
func ResolveMode(flagMode, cfgMode string, envKeyPresent bool) (string, error) {
	for _, c := range []struct{ src, v string }{{"--mode", flagMode}, {"linear.sync_mode", cfgMode}} {
		v := strings.ToLower(strings.TrimSpace(c.v))
		switch v {
		case "":
			continue
		case ModeMCP, ModeDirect:
			return v, nil
		default:
			return "", fmt.Errorf("%s %q is not valid (want %s or %s)", c.src, c.v, ModeMCP, ModeDirect)
		}
	}
	if envKeyPresent {
		return ModeDirect, nil
	}
	return ModeMCP, nil
}
