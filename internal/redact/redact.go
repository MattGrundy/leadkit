// Package redact removes secrets from text that is about to be shown to an agent or a log.
package redact

import "strings"

// URLCredentials removes the userinfo of every URL in s (`https://user:token@host/x` becomes
// `https://***@host/x`), so a remote that embeds a credential is never echoed. The userinfo is
// whatever precedes the LAST `@` of the URL's authority, which runs to the first `/`, whitespace
// or quote: a password with an unencoded `@` is removed whole. A scp-style remote
// (`token@host:path`) has no scheme and is left alone here; use Origin for a value known to be a
// remote.
func URLCredentials(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+3])
		s = s[i+3:]
		end := strings.IndexAny(s, "/ \t\r\n\"'`<>")
		if end < 0 {
			end = len(s)
		}
		auth := s[:end]
		if at := strings.LastIndexByte(auth, '@'); at >= 0 {
			b.WriteString("***@")
			auth = auth[at+1:]
		}
		b.WriteString(auth)
		s = s[end:]
	}
}

// Origin makes a git remote safe to show: URL userinfo is removed, and so is a scp-style `user@`
// unless the user is `git`, which is the conventional ssh user and no secret. The rest of the remote
// (host, path, `.git`) is kept as written.
func Origin(u string) string {
	u = URLCredentials(u)
	if strings.Contains(u, "://") {
		return u
	}
	at, colon, slash := strings.IndexByte(u, '@'), strings.IndexByte(u, ':'), strings.IndexByte(u, '/')
	if at >= 0 && (colon < 0 || at < colon) && (slash < 0 || at < slash) && u[:at] != "git" {
		return u[at+1:]
	}
	return u
}
