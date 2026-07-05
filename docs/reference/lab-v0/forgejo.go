package main

import "strings"

// forgejoHost is the single Forgejo instance lab can drive AFK runs against. A
// project whose origin remote is hosted here is AFK-runnable; anything else
// (github.com, gitlab.com, a bare local path) is not, because tea has nothing
// to target there.
const forgejoHost = "git.cloonar.com"

// forgejoInfo is the result of inspecting a project's origin remote: whether it
// lives on the Forgejo host and, if so, the owner/repo pair tea would target.
// lab caches one of these per project path (remotes don't change within a
// process lifetime).
type forgejoInfo struct {
	IsForgejo bool
	Owner     string
	Repo      string
}

// detectForgejo derives (isForgejo, owner, repo) from a git origin URL. It
// understands the two shapes git emits — SCP-like SSH (user@host:owner/repo[.git])
// and URL form (scheme://[user@]host[:port]/owner/repo[.git]) — and reports
// Forgejo only when the host is exactly forgejoHost AND an owner/repo pair is
// present. The username in the SSH form is deliberately NOT pinned: Cloonar
// clones use forgejo@…, but git@… (or any other) is equally valid.
func detectForgejo(originURL string) forgejoInfo {
	host, path, ok := splitRemote(strings.TrimSpace(originURL))
	if !ok || strings.ToLower(host) != forgejoHost {
		return forgejoInfo{}
	}
	owner, repo, ok := splitOwnerRepo(path)
	if !ok {
		return forgejoInfo{}
	}
	return forgejoInfo{IsForgejo: true, Owner: owner, Repo: repo}
}

// RepoURL is the project's Forgejo repo home — https://<forgejoHost>/<Owner>/<Repo>,
// the page the ⋯ menu's "Repository ↗" row links to. It reuses forgejoHost so the
// host stays a single source shared with detectForgejo, and returns "" for a
// non-Forgejo project: there is no git.cloonar.com URL to offer, so the template
// leaves such cards unchanged (the link renders only inside the {{if .Forgejo}}
// block).
func (f forgejoInfo) RepoURL() string {
	if !f.IsForgejo {
		return ""
	}
	return "https://" + forgejoHost + "/" + f.Owner + "/" + f.Repo
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
// segments — a Forgejo repo is owner/repo, so a one-segment or deeper path is
// not a repo lab can target.
func splitOwnerRepo(path string) (owner, repo string, ok bool) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
