package redact

import (
	"strings"
	"testing"
)

func TestURLCredentials(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@github.com/acme/w.git":             "https://***@github.com/acme/w.git",
		"https://tok@github.com/acme/w.git":                  "https://***@github.com/acme/w.git",
		"https://user:p@sstok@github.com/acme/w.git":         "https://***@github.com/acme/w.git", // an unencoded @ in the password
		"https://user:tok@host:8443/x":                       "https://***@host:8443/x",
		"ssh://git:tok@host/x.git and https://a@h/y":         "ssh://***@host/x.git and https://***@h/y",
		`origin "https://u:tok@github.com/o/r.git" mismatch`: `origin "https://***@github.com/o/r.git" mismatch`,
		"https://github.com/acme/w.git":                      "https://github.com/acme/w.git",
		"see https://github.com/a@b/c":                       "see https://github.com/a@b/c", // an @ in the PATH is not userinfo
		"https://user:tok@github.com":                        "https://***@github.com",
		"no url, tok@here":                                   "no url, tok@here",
		"":                                                   "",
	}
	for in, want := range cases {
		got := URLCredentials(in)
		if got != want {
			t.Errorf("URLCredentials(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "tok@") && strings.Contains(in, "://") && !strings.Contains(want, "tok@") {
			t.Errorf("URLCredentials(%q) kept a credential: %q", in, got)
		}
	}
}

func TestOrigin(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/w.git":              "git@github.com:acme/w.git", // the conventional ssh user is no secret
		"tok@github.com:acme/w.git":              "github.com:acme/w.git",
		"https://user:tok@github.com/acme/w.git": "https://***@github.com/acme/w.git",
		"https://github.com/acme/w.git":          "https://github.com/acme/w.git",
		"/local/path/repo":                       "/local/path/repo",
		"":                                       "",
	}
	for in, want := range cases {
		if got := Origin(in); got != want {
			t.Errorf("Origin(%q) = %q, want %q", in, got, want)
		}
	}
}
