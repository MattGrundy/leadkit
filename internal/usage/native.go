package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"mgpp.dev/leadkit/internal/burn"
)

// KeychainService is the -s (service name) usage-check.sh reads the OAuth credential
// from in the macOS Keychain. Exported so internal/config can source its
// keychain_service profile default from this one place instead of duplicating the
// literal.
const KeychainService = "Claude Code-credentials"

// usageAPIURL is the same OAuth-authenticated endpoint usage-check.sh (and Claude Code
// itself) calls.
const usageAPIURL = "https://api.anthropic.com/api/oauth/usage"

// nativeHTTPClient bounds the live-usage request so a network hiccup can't hang a burn
// tick indefinitely.
var nativeHTTPClient = &http.Client{Timeout: 10 * time.Second}

// keychainSecret mirrors the JSON `security find-generic-password -w` prints for the
// "Claude Code-credentials" item.
type keychainSecret struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// parseKeychainToken extracts the OAuth access token from the keychain secret JSON
// `security find-generic-password -w` prints. Pure and side-effect free, so it's
// directly unit-testable against fixture JSON without touching the real Keychain.
//
// SECURITY: the returned token must never be logged, printed, or wrapped into an
// error — callers own that obligation from here on.
func parseKeychainToken(data []byte) (string, error) {
	var sec keychainSecret
	if err := json.Unmarshal(data, &sec); err != nil {
		return "", fmt.Errorf("usage: parse keychain secret: %w", err)
	}
	if sec.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("usage: keychain secret has no accessToken")
	}
	return sec.ClaudeAiOauth.AccessToken, nil
}

// fetchKeychainToken runs `security find-generic-password -s "Claude Code-credentials"
// -w` and extracts the OAuth access token from its output. The command's own error
// path (exec's *ExitError) never carries stdout/the parsed token, so wrapping it here
// is safe.
func fetchKeychainToken() (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", KeychainService, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("usage: read keychain: %w", err)
	}
	return parseKeychainToken(out)
}

// fetchUsageAPIBody reads the OAuth token from the Keychain and immediately uses it to
// build and send the authenticated GET.
//
// SECURITY (hard): the token lives only in this function's local scope — set on the
// request header and then cleared — and is never logged, printed, or placed in a
// struct or error that could be marshalled or printed. Request/response error paths
// below report only the failure kind (status code, transport error), never req/resp
// themselves, since those carry the Authorization header.
func fetchUsageAPIBody() ([]byte, error) {
	token, err := fetchKeychainToken()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, usageAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("usage: build live-usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	token = "" //nolint:ineffassign // intentional: drop the bearer-token reference as soon as the header is set, so it isn't retained in a live local past this point

	resp, err := nativeHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage: fetch live usage: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage: live usage endpoint returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("usage: read live usage response: %w", err)
	}
	return body, nil
}

// FetchLiveNative fetches live account usage directly — no usage-check.sh dependency
// — by reading the OAuth token from the macOS Keychain and calling the same endpoint
// the script wraps. The response is the same JSON shape usage-check.sh --json prints,
// so it's parsed with the existing, already-tested ParseJSON.
func FetchLiveNative() (burn.Usage, error) {
	body, err := fetchUsageAPIBody()
	if err != nil {
		return burn.Usage{}, err
	}
	return ParseJSON(body)
}
