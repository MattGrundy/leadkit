package install

import "testing"

// TestOriginRepoName_ParsesAllURLFormsToSameName is the origin-URL parsing contract: the
// ssh scp-style, https, ssh://, and no-.git forms of the SAME repo all resolve to the
// SAME slug (the repo's stable identity), while owner and host segments are discarded.
func TestOriginRepoName_ParsesAllURLFormsToSameName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		// ssh scp-style, https, ssh://, git:// and no-.git forms → the same repo name.
		{"git@github.com:acmecorp/widget-core.git", "widget-core"},
		{"https://github.com/acmecorp/widget-core.git", "widget-core"},
		{"https://github.com/acmecorp/widget-core", "widget-core"},
		{"ssh://git@github.com/acmecorp/widget-core.git", "widget-core"},
		{"git://github.com/acmecorp/widget-core.git", "widget-core"},
		{"https://github.com/acmecorp/widget-core/", "widget-core"}, // trailing slash tolerated
		// A clone dir name that differs from the origin name is irrelevant — only the
		// origin's last segment matters.
		{"git@github.com:acmecorp/service-api.git", "service-api"},
		// Owner-less scp path (name right after the ':').
		{"git@github.com:widget-core.git", "widget-core"},
		// Mixed case slugifies to lowercase, matching every other leadkit slug.
		{"https://github.com/Acmecorp/Widget-Core.git", "widget-core"},
		// Whitespace around the URL (git output is trimmed).
		{"  git@github.com:acmecorp/widget-core.git\n", "widget-core"},
		// Malformed / empty → "" (caller falls back to basename, never crashes).
		{"", ""},
		{"   ", ""},
		{"not a url", "not-a-url"}, // no separators → whole string slugified (still no crash)
		{".git", ""},               // strips to empty
	}
	for _, c := range cases {
		if got := OriginRepoName(c.url); got != c.want {
			t.Errorf("OriginRepoName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
