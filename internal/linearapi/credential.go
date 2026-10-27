package linearapi

import (
	"errors"
	"os/exec"
	"strings"
)

// ErrNoKey is the refusal for direct mode with no key anywhere. It deliberately
// names no value and never triggers a silent fallback to MCP.
var ErrNoKey = errors.New("direct sync needs LINEAR_API_KEY in the env or the configured keychain entry; set it or use --mode mcp")

// KeychainReader fetches the secret for (service, account) from the OS keychain.
// Injected so tests never touch a real keychain.
type KeychainReader func(service, account string) (string, error)

// SecurityKeychain reads via macOS `security find-generic-password -w`. The key
// is captured only into the returned string (callers must not log it); the
// service/account are the only things on the command line.
func SecurityKeychain(service, account string) (string, error) {
	if service == "" {
		// An empty -s can match ambiguous entries and raise a GUI prompt that hangs
		// a non-interactive run (same guard as doctor's keychainCheck).
		return "", errors.New("no keychain_service configured")
	}
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", errors.New("keychain entry not reachable")
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveKey returns the Linear API key: env first, then the keychain
// (service/account). Blank values count as absent. Absent everywhere returns
// ErrNoKey. Errors never contain the key.
func ResolveKey(getenv func(string) string, kc KeychainReader, keychainService string) (string, error) {
	if k := strings.TrimSpace(getenv(EnvKey)); k != "" {
		return k, nil
	}
	if kc != nil && keychainService != "" {
		if k, err := kc(keychainService, KeychainAccount); err == nil && strings.TrimSpace(k) != "" {
			return strings.TrimSpace(k), nil
		}
	}
	return "", ErrNoKey
}

// KeyReachable reports whether ResolveKey would succeed, discarding the value —
// doctor's "reachable, value discarded" check.
func KeyReachable(getenv func(string) string, kc KeychainReader, keychainService string) bool {
	k, err := ResolveKey(getenv, kc, keychainService)
	_ = k
	return err == nil
}
