package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/prcreate"
	"mgpp.dev/leadkit/internal/registry"
)

var confirmFacts = prcreate.RetargetFacts{Repo: "acme/widget", PR: "4", From: "below", To: "main"}

func TestConfirmTyped_OnlyThePRNumberConfirms(t *testing.T) {
	for _, c := range []struct {
		name, typed string
		ok          bool
	}{
		{"the number", "4\n", true},
		{"the number with spaces", "  4 \n", true},
		{"the number without a newline at EOF", "4", true},
		{"yes", "yes\n", false},
		{"y", "y\n", false},
		{"another PR", "5\n", false},
		{"a prefix", "44\n", false},
		{"the number inside a sentence", "PR 4\n", false},
		{"empty line", "\n", false},
		{"nothing, then EOF", "", false},
	} {
		var out bytes.Buffer
		err := confirmTyped(strings.NewReader(c.typed), &out, confirmFacts)
		if (err == nil) != c.ok {
			t.Errorf("%s: confirmed=%v, want %v (err %v)", c.name, err == nil, c.ok, err)
		}
	}
}

func TestConfirmTyped_ShowsWhatIsBeingConfirmed(t *testing.T) {
	var out bytes.Buffer
	_ = confirmTyped(strings.NewReader("4\n"), &out, confirmFacts)
	for _, want := range []string{"acme/widget", "#4", "below -> main", "Type the PR number (4)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the owner must be shown %q:\n%s", want, out.String())
		}
	}
}

type noTTY struct{}

func (noTTY) Read([]byte) (int, error)    { return 0, io.EOF }
func (noTTY) Write(p []byte) (int, error) { return len(p), nil }
func (noTTY) Close() error                { return nil }

func TestConfirmRetargetAtTerminal_NoTerminalRefusesAndSaysWhy(t *testing.T) {
	orig := openTerminal
	openTerminal = func() (io.ReadWriteCloser, error) { return nil, errors.New("open /dev/tty: device not configured") }
	t.Cleanup(func() { openTerminal = orig })
	err := confirmRetargetAtTerminal(confirmFacts)
	if err == nil || !strings.Contains(err.Error(), "no controlling terminal") || !strings.Contains(err.Error(), "yourself in a terminal") {
		t.Fatalf("got %v", err)
	}
}

func TestConfirmRetargetAtTerminal_ReadsTheAnswerFromTheTerminalNotStdin(t *testing.T) {
	orig := openTerminal
	t.Cleanup(func() { openTerminal = orig })
	var shown bytes.Buffer
	openTerminal = func() (io.ReadWriteCloser, error) {
		return struct {
			io.Reader
			io.Writer
			io.Closer
		}{strings.NewReader("4\n"), &shown, noTTY{}}, nil
	}
	if err := confirmRetargetAtTerminal(confirmFacts); err != nil {
		t.Fatalf("a typed 4 on the terminal must confirm: %v", err)
	}
	if !strings.Contains(shown.String(), "below -> main") {
		t.Errorf("the prompt must go to the terminal:\n%s", shown.String())
	}
}

func TestLeadVerdict_BothTheSessionIdAndTheHostMustMatch(t *testing.T) {
	now := time.Now()
	live := func(id, host string, role registry.Role) registry.Entry {
		return registry.Entry{SessionID: id, Host: host, Role: role, Name: id, UpdatedAt: now}
	}
	cutoff := now.Add(-time.Hour)

	if ok, why := leadVerdict([]registry.Entry{live("me", "h1", registry.RoleLead)}, "h1", "me", cutoff); !ok {
		t.Fatalf("control: my own live lead entry reads as lead: %s", why)
	}
	// Another live lead exists, but it is not this session: sharing only the host, or only the id, is not enough.
	other := []registry.Entry{live("someone-else", "h1", registry.RoleLead)}
	if ok, _ := leadVerdict(other, "h1", "me", cutoff); ok {
		t.Error("a live lead on the same host with a different session id must not make me a lead")
	}
	sameID := []registry.Entry{live("me", "h2", registry.RoleLead)}
	if ok, _ := leadVerdict(sameID, "h1", "me", cutoff); ok {
		t.Error("a live lead with my session id on another host must not make me a lead")
	}
	if ok, why := leadVerdict([]registry.Entry{live("me", "h1", registry.RoleIC)}, "h1", "me", cutoff); ok || !strings.Contains(why, `"ic"`) {
		t.Errorf("an ic is refused and the role named: %v %s", ok, why)
	}
	stale := registry.Entry{SessionID: "me", Host: "h1", Role: registry.RoleLead, UpdatedAt: now.Add(-2 * time.Hour)}
	if ok, _ := leadVerdict([]registry.Entry{stale}, "h1", "me", cutoff); ok {
		t.Error("a stale lead entry is not live")
	}
}
