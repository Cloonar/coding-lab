package gitx

import (
	"net/url"
	"path"
	"strings"
)

// SanitizeRepoName ports v0's sessionName sanitizer (scanner.go — the rule
// every session name, branch, and worktree dir leans on): trim surrounding
// whitespace, replace '/' with '-' first, then per BYTE (not rune) keep
// [A-Za-z0-9_-] and replace everything else with '_'. A multi-byte
// character becomes one '_' per byte.
//
// '.' is deliberately NOT in the safe set, even though it is a legal
// filename character: tmux rewrites '.' (and ':') to '_' in a session name
// at creation, while its target-pane parser reads a bare '.' as the
// window.pane separator — a repo "foo.bar" would be created as session
// "foo_bar" yet looked up as "foo.bar", so every has-session/capture-pane/
// send-keys would fail. Mapping '.' to '_' here mirrors exactly what tmux
// does, keeping lab's stored name and tmux's real session name in sync.
func SanitizeRepoName(raw string) string {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "/", "-")
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// NameFromURL derives a repo name from a remote URL: the path basename
// minus a ".git" suffix, passed through SanitizeRepoName. It understands
// scheme URLs (https://, ssh://, file://…), scp-like ssh remotes
// (git@host:owner/repo.git), and plain local paths. A degenerate URL with
// no usable basename yields "" — the caller rejects that (repo create
// 400s on an empty name).
func NameFromURL(remoteURL string) string {
	s := strings.TrimRight(strings.TrimSpace(remoteURL), "/")
	var base string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		base = path.Base(u.Path)
	default:
		if host, rest, ok := strings.Cut(s, ":"); ok && !strings.Contains(host, "/") {
			// scp-like: the first ':' separates host from path.
			base = path.Base(rest)
		} else {
			base = path.Base(s)
		}
	}
	if base == "." || base == "/" {
		return ""
	}
	return SanitizeRepoName(strings.TrimSuffix(base, ".git"))
}
