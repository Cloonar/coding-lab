// Package tracker holds the tracker seam shared by the forge and built-in
// implementations. This file is the forge-kind detection run once at
// repo-add time (design §3a: repos.forge_kind): the remote-URL parsing is
// ported verbatim from the v0 reference (docs/reference/lab-v0/forgejo.go),
// generalized from v0's single is-Forgejo bool to the three-valued
// forgejo|github|none vocabulary. forge_kind is the auto-binding hint and the
// registry's mismatch tripwire; the credential's flavor (ADR-0015) is what
// actually routes, so a 'none' host still resolves to a forge tracker when the
// operator binds one explicitly.
package tracker

import "strings"

// ForgeKind is the detected forge hosting a repo's remote — the
// repos.forge_kind vocabulary.
type ForgeKind string

const (
	ForgeKindForgejo ForgeKind = "forgejo"
	ForgeKindGitHub  ForgeKind = "github"
	ForgeKindNone    ForgeKind = "none"
)

// forgejoHost is the single Forgejo instance lab can drive (v0 pin). A repo
// whose remote is hosted here can bind its tracker to the Forgejo REST
// client; github.com is detected too, and anything else (gitlab.com, a bare
// local path) is ForgeKindNone because lab has no REST surface to target
// there.
const forgejoHost = "git.cloonar.com"

// githubHost extends the v0 host gate: github.com remotes record
// forge_kind 'github' at add time.
const githubHost = "github.com"

// ForgeInfo is the result of inspecting a repo's remote URL: which forge
// kind hosts it and, when detection succeeded, the owner/repo pair the REST
// client targets.
type ForgeInfo struct {
	Kind  ForgeKind
	Owner string
	Repo  string
}

// Detect derives the forge kind and owner/repo pair from a git remote URL.
// It understands the two shapes git emits — SCP-like SSH
// (user@host:owner/repo[.git]) and URL form
// (scheme://[user@]host[:port]/owner/repo[.git]) — and reports a forge only
// when the host matches exactly (case-insensitively) AND an owner/repo pair
// is present. The SSH username is deliberately NOT pinned: Cloonar clones
// use forgejo@…, but git@… (or any other) is equally valid.
func Detect(remoteURL string) ForgeInfo {
	host, path, ok := splitRemote(strings.TrimSpace(remoteURL))
	if !ok {
		return ForgeInfo{Kind: ForgeKindNone}
	}
	var kind ForgeKind
	switch strings.ToLower(host) {
	case forgejoHost:
		kind = ForgeKindForgejo
	case githubHost:
		kind = ForgeKindGitHub
	default:
		return ForgeInfo{Kind: ForgeKindNone}
	}
	owner, repo, ok := splitOwnerRepo(path)
	if !ok {
		return ForgeInfo{Kind: ForgeKindNone}
	}
	return ForgeInfo{Kind: kind, Owner: owner, Repo: repo}
}

// RepoPath extracts the "{owner}/{repo}" path segments a forge REST client
// uses, from any remote URL shape git emits. It is host-agnostic — callers
// gate on the detected forge kind; RepoPath only answers whether the remote
// path is exactly two non-empty segments. ok=false when it is not.
func RepoPath(remoteURL string) (string, bool) {
	_, path, ok := splitRemote(strings.TrimSpace(remoteURL))
	if !ok {
		return "", false
	}
	owner, repo, ok := splitOwnerRepo(path)
	if !ok {
		return "", false
	}
	return owner + "/" + repo, true
}

// splitRemote separates a git remote URL into its host and the path that
// follows it. URL form (anything containing "://") is tried first so its
// "scheme:" colon isn't mistaken for the SCP form's host/path separator.
func splitRemote(url string) (host, path string, ok bool) {
	if i := strings.Index(url, "://"); i >= 0 {
		rest := url[i+3:] // [user@]host[:port]/path
		// Strip userinfo only when the "@" precedes the first "/", so an "@" in
		// the path can't be mistaken for a userinfo separator.
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			if slash := strings.IndexByte(rest, '/'); slash < 0 || at < slash {
				rest = rest[at+1:]
			}
		}
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "", "", false
		}
		return stripPort(rest[:slash]), rest[slash+1:], true
	}
	// SCP-like: [user@]host:path — no scheme, no port, the first ":" splits it.
	if at := strings.IndexByte(url, '@'); at >= 0 {
		url = url[at+1:]
	}
	colon := strings.IndexByte(url, ':')
	if colon < 0 {
		return "", "", false
	}
	return url[:colon], url[colon+1:], true
}

func stripPort(hostport string) string {
	if i := strings.IndexByte(hostport, ':'); i >= 0 {
		return hostport[:i]
	}
	return hostport
}

// splitOwnerRepo pulls the owner and repo out of a remote path, tolerating a
// leading slash and a trailing ".git". It requires exactly two non-empty
// segments — a forge repo is owner/repo, so a one-segment or deeper path is
// not a repo lab can target.
func splitOwnerRepo(path string) (owner, repo string, ok bool) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
