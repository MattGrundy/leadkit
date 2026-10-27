package prcreate

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// Ticket 197. "No authority matched" has two causes that need different words: a profile that was
// read and has no entry, and a profile that was never found. Neither may tell an agent to add an
// authority entry: grants are the repo owner's.
func TestNoAuthority_TheTwoCausesAreToldApartAndNeitherAdvisesAGrant(t *testing.T) {
	const origin = "https://github.com/acme/widgets.git"
	const missing = "no leadkit profile was found (looked for X); HOME=\"/tmp\""
	reasons := map[string]func(profileMissing string) string{
		"create": func(m string) string { return Decide(DecideInput{OriginURL: origin, ProfileMissing: m}).Reason },
		"edit":   func(m string) string { return DecideEdit(DecideEditInput{OriginURL: origin, ProfileMissing: m}).Reason },
		"merge": func(m string) string {
			return DecideMerge(DecideMergeInput{OriginURL: origin, ProfileMissing: m}).Reason
		},
	}
	for verb, reason := range reasons {
		read := reason("")
		if !strings.Contains(read, "no authority.<repo> policy matches origin "+origin) || !strings.Contains(read, "ask the repo owner") {
			t.Errorf("%s, profile read: want the no-match fact and 'ask the repo owner', got: %s", verb, read)
		}
		notFound := reason(missing)
		if !strings.Contains(notFound, missing) || strings.Contains(notFound, "no authority.<repo> policy matches") {
			t.Errorf("%s, profile not found: want the search named and no claim that a policy was consulted, got: %s", verb, notFound)
		}
		for _, r := range []string{read, notFound} {
			if strings.Contains(r, "add an authority entry") {
				t.Errorf("%s: a refusal must not advise adding an authority entry: %s", verb, r)
			}
		}
	}
}

// An origin can carry a credential; no message built from it may print it.
func TestMessagesBuiltFromAnOriginNeverPrintItsCredential(t *testing.T) {
	const origin = "https://deploy:SECRETTOK123@git.internal.example/org/w.git"
	var msgs []string
	msgs = append(msgs, noAuthorityReason("PR-create", origin, ""), noAuthorityReason("PR-create", origin, "no profile"))
	if _, err := ForgeName(origin, config.AuthoritySpec{}); err != nil {
		msgs = append(msgs, err.Error())
	}
	if _, err := ForgeName(origin, config.AuthoritySpec{Forge: "bogus"}); err != nil {
		msgs = append(msgs, err.Error())
	}
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages to check (both forge errors must fire), got %d", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m, "SECRETTOK123") || strings.Contains(m, "deploy:") {
			t.Errorf("a message leaks the remote's credential: %s", m)
		}
		if !strings.Contains(m, "git.internal.example/org/w.git") && !strings.Contains(m, "no profile") {
			t.Errorf("the message should still name the host and path: %s", m)
		}
	}
}
