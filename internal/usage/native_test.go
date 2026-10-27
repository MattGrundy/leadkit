package usage

import "testing"

const fixtureKeychainJSON = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-fixture-token","refreshToken":"sk-ant-ort01-fixture","expiresAt":1234567890}}`

func TestParseKeychainToken(t *testing.T) {
	got, err := parseKeychainToken([]byte(fixtureKeychainJSON))
	if err != nil {
		t.Fatalf("parseKeychainToken: %v", err)
	}
	if got != "sk-ant-oat01-fixture-token" {
		t.Errorf("token = %q, want %q", got, "sk-ant-oat01-fixture-token")
	}
}

func TestParseKeychainToken_MissingToken(t *testing.T) {
	if _, err := parseKeychainToken([]byte(`{"claudeAiOauth":{}}`)); err == nil {
		t.Fatal("want error for a secret with no accessToken, got nil")
	}
}

func TestParseKeychainToken_InvalidJSON(t *testing.T) {
	if _, err := parseKeychainToken([]byte("not json")); err == nil {
		t.Fatal("want error for invalid json, got nil")
	}
}

// The usage-API response parse step reuses ParseJSON (the endpoint returns the same
// JSON shape usage-check.sh --json prints — see native.go's FetchLiveNative doc) —
// TestParseJSON in usage_test.go already asserts SessionPct/WeekPct map correctly
// from that fixture, so it isn't duplicated here.
