package main

import "testing"

func TestDetectForgejo(t *testing.T) {
	for _, tc := range []struct {
		name        string
		url         string
		want        bool
		owner, repo string
	}{
		// Forgejo host, every remote shape git emits.
		{"scp form, forgejo user (real Cloonar origin)", "forgejo@git.cloonar.com:Cloonar/nixos.git", true, "Cloonar", "nixos"},
		{"scp form, git user", "git@git.cloonar.com:Cloonar/nixos.git", true, "Cloonar", "nixos"},
		{"scp form, no .git suffix", "git@git.cloonar.com:Cloonar/nixos", true, "Cloonar", "nixos"},
		{"https form", "https://git.cloonar.com/Cloonar/nixos.git", true, "Cloonar", "nixos"},
		{"https form, no .git", "https://git.cloonar.com/Cloonar/nixos", true, "Cloonar", "nixos"},
		{"https with userinfo", "https://forgejo@git.cloonar.com/Cloonar/nixos.git", true, "Cloonar", "nixos"},
		{"ssh url form", "ssh://git@git.cloonar.com/Cloonar/nixos.git", true, "Cloonar", "nixos"},
		{"ssh url form with port", "ssh://git@git.cloonar.com:22/Cloonar/nixos.git", true, "Cloonar", "nixos"},
		{"https trailing slash", "https://git.cloonar.com/Cloonar/nixos/", true, "Cloonar", "nixos"},
		{"host case-insensitive", "git@GIT.CLOONAR.COM:Cloonar/nixos.git", true, "Cloonar", "nixos"},

		// Other hosts → not Forgejo.
		{"github https", "https://github.com/foo/bar.git", false, "", ""},
		{"github scp", "git@github.com:foo/bar.git", false, "", ""},
		{"gitlab scp", "git@gitlab.com:foo/bar.git", false, "", ""},

		// Forgejo host but not an owner/repo pair.
		{"owner only", "https://git.cloonar.com/Cloonar", false, "", ""},
		{"host only", "https://git.cloonar.com/", false, "", ""},
		{"too deep (no nested repos)", "https://git.cloonar.com/Cloonar/sub/nixos.git", false, "", ""},

		// Degenerate inputs.
		{"empty", "", false, "", ""},
		{"garbage", "not a url", false, "", ""},
		{"local path", "/home/dominik/projects/thing", false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detectForgejo(tc.url)
			if got.IsForgejo != tc.want || got.Owner != tc.owner || got.Repo != tc.repo {
				t.Errorf("detectForgejo(%q) = %+v; want {IsForgejo:%v Owner:%q Repo:%q}",
					tc.url, got, tc.want, tc.owner, tc.repo)
			}
		})
	}
}

func TestForgejoInfoRepoURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		info forgejoInfo
		want string
	}{
		{"forgejo repo → repo home on forgejoHost", forgejoInfo{IsForgejo: true, Owner: "Cloonar", Repo: "nixos"}, "https://git.cloonar.com/Cloonar/nixos"},
		{"not forgejo → empty (no URL to offer)", forgejoInfo{}, ""},
		// Defensive: owner/repo set but IsForgejo false (can't arise from
		// detectForgejo, but the gate is IsForgejo, not the fields).
		{"not forgejo even with owner/repo populated", forgejoInfo{Owner: "x", Repo: "y"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.RepoURL(); got != tc.want {
				t.Errorf("RepoURL() = %q; want %q", got, tc.want)
			}
		})
	}

	// Round-trip from a real origin: every Forgejo remote shape resolves to the
	// same repo home, so the menu link is stable across clone styles.
	for _, url := range []string{
		"forgejo@git.cloonar.com:Cloonar/nixos.git",
		"https://git.cloonar.com/Cloonar/nixos",
		"ssh://git@git.cloonar.com:22/Cloonar/nixos.git",
	} {
		if got := detectForgejo(url).RepoURL(); got != "https://git.cloonar.com/Cloonar/nixos" {
			t.Errorf("detectForgejo(%q).RepoURL() = %q; want the repo home", url, got)
		}
	}
}
