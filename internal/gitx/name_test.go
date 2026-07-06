package gitx

import "testing"

func TestSanitizeRepoName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// The v0 scanner table (portspec store-http-ui §2.1).
		{"plain", "plain"},
		{"a/b", "a-b"},
		{"with space", "with_space"},
		{"weird:chars*here", "weird_chars_here"},
		{"dot.keep_and-dash", "dot_keep_and-dash"},

		// sanitizeLabel behaviors carried over: trim, tilde, emptiness.
		{"  trimmed  ", "trimmed"},
		{"tilde~here", "tilde_here"},
		{"", ""},
		{"   ", ""},

		// Byte loop, not rune loop: one '_' per byte of a multi-byte char.
		{"café", "caf__"},
	}
	for _, tt := range tests {
		if got := SanitizeRepoName(tt.in); got != tt.want {
			t.Errorf("SanitizeRepoName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://git.cloonar.com/Cloonar/coding-lab.git", "coding-lab"},
		{"https://git.cloonar.com/Cloonar/coding-lab", "coding-lab"},
		{"https://git.cloonar.com/Cloonar/coding-lab/", "coding-lab"},
		{"git@git.cloonar.com:Cloonar/coding-lab.git", "coding-lab"},
		{"git@github.com:owner/repo", "repo"},
		{"host:repo.git", "repo"},
		{"ssh://git@git.cloonar.com:2222/owner/repo.git", "repo"},
		{"file:///tmp/fixtures/origin.git", "origin"},
		{"/local/path/repo.git", "repo"},
		{"/local/path/my repo", "my_repo"},
		{"https://host/owner/we.ird.git", "we_ird"},

		// Degenerate inputs yield "" (the API rejects an empty name).
		{"", ""},
		{"https://host/", ""},
		{"/", ""},
	}
	for _, tt := range tests {
		if got := NameFromURL(tt.url); got != tt.want {
			t.Errorf("NameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}
