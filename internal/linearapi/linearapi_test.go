package linearapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveMode_Precedence(t *testing.T) {
	cases := []struct {
		flag, cfg string
		envKey    bool
		want      string
	}{
		{"", "", false, ModeMCP},             // default: today's behaviour
		{"", "", true, ModeDirect},           // env key auto-selects direct
		{"mcp", "", true, ModeMCP},           // flag forces mcp despite key
		{"", "mcp", true, ModeMCP},           // config forces mcp despite key
		{"direct", "mcp", false, ModeDirect}, // flag > config
		{"", "direct", false, ModeDirect},    // config forces direct (key checked later, loudly)
		{"mcp", "direct", true, ModeMCP},     // flag > config
	}
	for _, c := range cases {
		got, err := ResolveMode(c.flag, c.cfg, c.envKey)
		if err != nil || got != c.want {
			t.Errorf("ResolveMode(%q,%q,%v) = %q,%v want %q", c.flag, c.cfg, c.envKey, got, err, c.want)
		}
	}
	if _, err := ResolveMode("bogus", "", false); err == nil {
		t.Error("an invalid mode must be refused, not defaulted")
	}
}

func TestResolveKey_EnvBeatsKeychain_AbsentRefuses(t *testing.T) {
	kc := func(service, account string) (string, error) {
		if service != "svc" || account != KeychainAccount {
			t.Errorf("keychain asked for (%q,%q)", service, account)
		}
		return "kc-key", nil
	}
	if k, _ := ResolveKey(env(map[string]string{EnvKey: "env-key"}), kc, "svc"); k != "env-key" {
		t.Errorf("env must win over keychain, got %q", k)
	}
	if k, _ := ResolveKey(env(nil), kc, "svc"); k != "kc-key" {
		t.Errorf("keychain is the fallback, got %q", k)
	}
	if k, _ := ResolveKey(env(map[string]string{EnvKey: "   "}), kc, "svc"); k != "kc-key" {
		t.Errorf("a blank env key counts as absent, got %q", k)
	}
	noKC := func(string, string) (string, error) { return "", errors.New("nope") }
	_, err := ResolveKey(env(nil), noKC, "svc")
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("absent key must refuse with ErrNoKey, got %v", err)
	}
	if !strings.Contains(err.Error(), "--mode mcp") {
		t.Errorf("refusal should point at the mcp escape hatch: %v", err)
	}
	if KeyReachable(env(nil), noKC, "svc") {
		t.Error("KeyReachable must be false when absent")
	}
	if !KeyReachable(env(map[string]string{EnvKey: "x"}), nil, "") {
		t.Error("KeyReachable must be true with an env key")
	}
}

type fakeDoer func(*http.Request) (*http.Response, error)

func (f fakeDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, body string, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestClient_SendsRawKeyAndBacksOffOnRateLimit(t *testing.T) {
	calls := 0
	var slept []time.Duration
	c := New("secret-key", fakeDoer(func(r *http.Request) (*http.Response, error) {
		calls++
		if got := r.Header.Get("Authorization"); got != "secret-key" {
			t.Errorf("Authorization = %q, want the raw key (Linear API keys take no Bearer prefix)", got)
		}
		if r.URL.String() != Endpoint {
			t.Errorf("url = %s", r.URL)
		}
		if calls == 1 {
			return resp(429, `{}`, map[string]string{"Retry-After": "3"}), nil
		}
		if calls == 2 {
			return resp(200, `{"errors":[{"message":"slow down","extensions":{"code":"RATELIMITED"}}]}`, nil), nil
		}
		return resp(200, `{"data":{"ok":true}}`, nil), nil
	}))
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	var out struct{ OK bool }
	if err := c.Do(context.Background(), "query{x}", nil, &out); err != nil || !out.OK {
		t.Fatalf("Do = %v, %+v", err, out)
	}
	if calls != 3 || len(slept) != 2 {
		t.Fatalf("calls=%d slept=%v", calls, slept)
	}
	if slept[0] != 3*time.Second || slept[1] != 2*time.Second {
		t.Errorf("backoff = %v, want Retry-After honoured then doubling", slept)
	}
}

func TestClient_GivesUpAfterMaxRetries(t *testing.T) {
	c := New("k", fakeDoer(func(*http.Request) (*http.Response, error) { return resp(429, `{}`, nil), nil }))
	c.MaxRetries = 2
	c.Sleep = func(time.Duration) {}
	if err := c.Do(context.Background(), "q", nil, nil); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("want a rate-limit give-up error, got %v", err)
	}
}

func TestClient_KeyNeverEscapesInErrorsOrFormatting(t *testing.T) {
	const key = "lin_api_SUPERSECRET"
	// A transport error and a server that echoes the key back, both scrubbed.
	for name, d := range map[string]fakeDoer{
		"transport": func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("dial failed using %s", key) },
		"echo": func(*http.Request) (*http.Response, error) {
			return resp(200, `{"errors":[{"message":"bad auth `+key+`"}]}`, nil), nil
		},
		"http": func(*http.Request) (*http.Response, error) { return resp(500, key, nil), nil },
	} {
		c := New(key, d)
		err := c.Do(context.Background(), "q", nil, nil)
		if err == nil || strings.Contains(err.Error(), key) {
			t.Errorf("%s: error must exist and not contain the key: %v", name, err)
		}
	}
	c := New(key, nil)
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if s := fmt.Sprintf(f, c); strings.Contains(s, key) {
			t.Errorf("format %s leaks the key: %s", f, s)
		}
	}
}

func TestClient_BatchAliasesOpsAndPaginateExhausts(t *testing.T) {
	var q string
	c := New("k", fakeDoer(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		q = string(b)
		return resp(200, `{"data":{"op0":{"success":true},"op1":{"success":true}}}`, nil), nil
	}))
	res, err := c.Batch(context.Background(), []BatchOp{
		{Call: "issueUpdate(id: $id#, input: $in#)", Decl: "$id#: String!, $in#: IssueUpdateInput!", Vars: map[string]any{"id#": "a", "in#": 1}, Sel: "success"},
		{Call: "issueUpdate(id: $id#, input: $in#)", Decl: "$id#: String!, $in#: IssueUpdateInput!", Vars: map[string]any{"id#": "b", "in#": 2}, Sel: "success"},
	})
	if err != nil || len(res) != 2 {
		t.Fatalf("Batch = %v, %v", res, err)
	}
	for _, want := range []string{"op0: issueUpdate(id: $id0, input: $in0)", "op1: issueUpdate(id: $id1, input: $in1)", `"id1":"b"`} {
		if !strings.Contains(q, want) {
			t.Errorf("batched request missing %q:\n%s", want, q)
		}
	}
	pages := [][]int{{1, 2}, {3}}
	i := 0
	all, err := Paginate(func(after string) ([]int, string, bool, error) {
		p := pages[i]
		i++
		return p, fmt.Sprint("c", i), i < len(pages), nil
	})
	if err != nil || len(all) != 3 {
		t.Errorf("Paginate = %v, %v", all, err)
	}
}
