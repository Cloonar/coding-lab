package tracker

import "testing"

// TestDetect transcribes the full 19-row v0 TestDetectForgejo table
// (docs/reference/lab-v0/forgejo_test.go) into the three-valued forge-kind
// vocabulary, plus the new github rows. The two v0 github rows flip from
// "not Forgejo" to ForgeKindGitHub with owner/repo extracted.
func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name        string
		url         string
		kind        ForgeKind
		owner, repo string
	}{
		// Forgejo host, every remote shape git emits (v0 rows, unchanged).
		{"scp form, forgejo user (real Cloonar origin)", "forgejo@git.cloonar.com:Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},
		{"scp form, git user", "git@git.cloonar.com:Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},
		{"scp form, no .git suffix", "git@git.cloonar.com:Cloonar/nixos", ForgeKindForgejo, "Cloonar", "nixos"},
		{"https form", "https://git.cloonar.com/Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},
		{"https form, no .git", "https://git.cloonar.com/Cloonar/nixos", ForgeKindForgejo, "Cloonar", "nixos"},
		{"https with userinfo", "https://forgejo@git.cloonar.com/Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},
		{"ssh url form", "ssh://git@git.cloonar.com/Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},
		{"ssh url form with port", "ssh://git@git.cloonar.com:22/Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},
		{"https trailing slash", "https://git.cloonar.com/Cloonar/nixos/", ForgeKindForgejo, "Cloonar", "nixos"},
		{"host case-insensitive", "git@GIT.CLOONAR.COM:Cloonar/nixos.git", ForgeKindForgejo, "Cloonar", "nixos"},

		// github.com → 'github' (rewrite extension; v0 reported false here).
		{"github https", "https://github.com/foo/bar.git", ForgeKindGitHub, "foo", "bar"},
		{"github scp", "git@github.com:foo/bar.git", ForgeKindGitHub, "foo", "bar"},
		{"github https, no .git", "https://github.com/foo/bar", ForgeKindGitHub, "foo", "bar"},
		{"github ssh url form with port", "ssh://git@github.com:22/foo/bar.git", ForgeKindGitHub, "foo", "bar"},
		{"github trailing slash", "https://github.com/foo/bar/", ForgeKindGitHub, "foo", "bar"},
		{"github host case-insensitive", "git@GitHub.com:foo/bar.git", ForgeKindGitHub, "foo", "bar"},
		{"github owner only", "https://github.com/foo", ForgeKindNone, "", ""},
		{"github too deep", "https://github.com/a/b/c.git", ForgeKindNone, "", ""},

		// Other hosts → none.
		{"gitlab scp", "git@gitlab.com:foo/bar.git", ForgeKindNone, "", ""},

		// Forgejo host but not an owner/repo pair.
		{"owner only", "https://git.cloonar.com/Cloonar", ForgeKindNone, "", ""},
		{"host only", "https://git.cloonar.com/", ForgeKindNone, "", ""},
		{"too deep (no nested repos)", "https://git.cloonar.com/Cloonar/sub/nixos.git", ForgeKindNone, "", ""},

		// Degenerate inputs.
		{"empty", "", ForgeKindNone, "", ""},
		{"garbage", "not a url", ForgeKindNone, "", ""},
		{"local path", "/home/dominik/projects/thing", ForgeKindNone, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(tc.url)
			if got.Kind != tc.kind || got.Owner != tc.owner || got.Repo != tc.repo {
				t.Errorf("Detect(%q) = %+v; want {Kind:%q Owner:%q Repo:%q}",
					tc.url, got, tc.kind, tc.owner, tc.repo)
			}
		})
	}
}

func TestRepoPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
		ok   bool
	}{
		// Every remote shape resolves to the same owner/repo — the REST path
		// is stable across clone styles (v0 round-trip property).
		{"scp form", "forgejo@git.cloonar.com:Cloonar/nixos.git", "Cloonar/nixos", true},
		{"https form", "https://git.cloonar.com/Cloonar/nixos", "Cloonar/nixos", true},
		{"ssh url form with port", "ssh://git@git.cloonar.com:22/Cloonar/nixos.git", "Cloonar/nixos", true},
		{"https trailing slash", "https://git.cloonar.com/Cloonar/nixos/", "Cloonar/nixos", true},
		{"github https", "https://github.com/foo/bar.git", "foo/bar", true},
		// Host-agnostic: the caller gates on forge kind, not RepoPath.
		{"unknown host still parses", "git@gitlab.com:foo/bar.git", "foo/bar", true},
		// No owner/repo pair → no path.
		{"owner only", "https://git.cloonar.com/Cloonar", "", false},
		{"host only", "https://git.cloonar.com/", "", false},
		{"too deep", "https://git.cloonar.com/Cloonar/sub/nixos.git", "", false},
		{"empty", "", "", false},
		{"garbage", "not a url", "", false},
		{"local path", "/home/dominik/projects/thing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RepoPath(tc.url)
			if got != tc.want || ok != tc.ok {
				t.Errorf("RepoPath(%q) = (%q, %v); want (%q, %v)", tc.url, got, ok, tc.want, tc.ok)
			}
		})
	}
}
