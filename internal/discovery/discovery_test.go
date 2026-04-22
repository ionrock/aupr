package discovery

import "testing"

func TestParseGitHub(t *testing.T) {
	cases := []struct {
		url, owner, name string
		ok               bool
	}{
		{"git@github.com:dagster-io/aupr.git", "dagster-io", "aupr", true},
		{"https://github.com/dagster-io/aupr", "dagster-io", "aupr", true},
		{"https://github.com/dagster-io/aupr.git", "dagster-io", "aupr", true},
		{"ssh://git@github.com/dagster-io/internal.git", "dagster-io", "internal", true},
		{"git@gitlab.com:x/y.git", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		o, n, ok := parseGitHub(c.url)
		if ok != c.ok || o != c.owner || n != c.name {
			t.Errorf("parseGitHub(%q) = (%q,%q,%v), want (%q,%q,%v)", c.url, o, n, ok, c.owner, c.name, c.ok)
		}
	}
}
