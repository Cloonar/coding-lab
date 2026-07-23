// Package imageref parses OCI image references and resolves tags to
// digests against a registry's v2 API, so refs are stored digest-pinned
// (issue #207): the repo-settings save path feeds operator input like
// docker.io/debian:bookworm through Resolver.Pin and stores the canonical
// host/path:tag@sha256:… form. Pinning at save time is the point — a tag
// is a moving reference (ADR-0051 pins the same lesson for the agent-tools
// image), so the digest recorded on save is exactly what the spawn-time
// pull runs, and a same-tag re-push between save and spawn changes nothing.
//
// Refs must be fully qualified. Podman-style short-name resolution
// ("debian" → whatever unqualified-search-registries says) is machine-local
// policy, and a stored ref has to mean the same image on every host, so the
// first path segment must look like a registry host (a dot, a port, or
// "localhost"). The one shorthand kept is docker.io's library/ namespace:
// docker.io/debian is unambiguous and normalizes to docker.io/library/debian
// (index.docker.io normalizes to docker.io first).
//
// Parse errors surface verbatim in the repo-settings form banner, so every
// rejection names the part that is wrong (bad tag, bad digest, uppercase
// path, missing host) instead of a bare "invalid reference". Parse and the
// Ref accessors are pure; Resolver (resolve.go) is the only thing that
// touches the network.
package imageref

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The reference grammar, per the docker/OCI distribution spec: lowercase
// path components joined by single dots, underscores (one or doubled) or
// runs of dashes; a 128-char-max tag that cannot start with a dot or dash;
// a digest that is exactly sha256 + 64 lowercase hex. The host is the usual
// DNS-name-or-IP shape with an optional :port — uppercase is tolerated
// there (DNS is case-insensitive) but nowhere else.
var (
	hostRE          = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?(?::[0-9]{1,5})?$`)
	pathComponentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$`)
	tagRE           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRE        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Ref is a parsed image reference. Host may carry a :port; Path is the
// repository path (docker.io refs always carry the library/ prefix after
// Parse); Tag may be empty only when Digest is set (a digest-only ref);
// Digest is empty or "sha256:" + 64 lowercase hex.
type Ref struct {
	Host   string
	Path   string
	Tag    string
	Digest string
}

// Pinned reports whether the ref carries a digest — a pinned ref needs no
// registry round-trip and is immune to tag re-pushes.
func (r Ref) Pinned() bool { return r.Digest != "" }

// String renders the canonical form host/path[:tag][@digest]. When both
// tag and digest are present the tag is kept: podman accepts
// name:tag@digest (the digest wins for pulling), and the tag keeps the
// stored ref human-readable in the settings UI.
func (r Ref) String() string {
	var b strings.Builder
	b.WriteString(r.Host)
	b.WriteByte('/')
	b.WriteString(r.Path)
	if r.Tag != "" {
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}
	if r.Digest != "" {
		b.WriteByte('@')
		b.WriteString(r.Digest)
	}
	return b.String()
}

// Parse validates s against the reference grammar and returns the
// normalized Ref: index.docker.io → docker.io, docker.io single-segment
// paths gain the library/ prefix, and a ref with neither tag nor digest
// defaults to :latest. Digest-only refs keep Tag empty. Error strings are
// user-facing (repo-settings form banner) and name the offending part.
func Parse(s string) (Ref, error) {
	if s == "" {
		return Ref{}, errors.New("image ref is empty")
	}
	if strings.Contains(s, "://") {
		return Ref{}, fmt.Errorf("image ref %q must not include a scheme: write host/path[:tag], e.g. docker.io/library/debian:bookworm", s)
	}
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return Ref{}, fmt.Errorf("image ref %q must be fully qualified, e.g. docker.io/library/debian:bookworm", s)
	}
	host, rest := s[:slash], s[slash+1:]
	// The fully-qualified gate: the first segment must look like a registry
	// host, not a namespace — "foo/bar" is a short name, not a location.
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return Ref{}, fmt.Errorf("image ref %q must be fully qualified — %q is not a registry host; write e.g. docker.io/library/debian:bookworm", s, host)
	}
	if !hostRE.MatchString(host) {
		return Ref{}, fmt.Errorf("invalid registry host %q: letters, digits, dots and dashes with an optional :port", host)
	}

	// Digest first: '@' can only introduce a digest, and splitting it off
	// before the tag keeps the digest's own colon out of the tag split.
	var digest string
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		digest, rest = rest[at+1:], rest[:at]
		if digest == "" {
			return Ref{}, errors.New("image ref has an empty digest after '@'")
		}
		if !digestRE.MatchString(digest) {
			return Ref{}, fmt.Errorf("invalid digest %q: want \"sha256:\" followed by 64 lowercase hex digits", digest)
		}
	}

	// Path components never contain ':', so any colon left starts the tag.
	var tag string
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		tag, rest = rest[colon+1:], rest[:colon]
		if tag == "" {
			return Ref{}, errors.New("image ref has an empty tag after ':'")
		}
		if !tagRE.MatchString(tag) {
			return Ref{}, fmt.Errorf("invalid tag %q: up to 128 letters, digits, underscores, dots and dashes, not starting with a dot or dash", tag)
		}
	}

	path := rest
	if path == "" {
		return Ref{}, fmt.Errorf("image ref %q has no repository path after the host", s)
	}
	for _, comp := range strings.Split(path, "/") {
		if comp == "" {
			return Ref{}, fmt.Errorf("invalid repository path %q: empty path component", path)
		}
		if !pathComponentRE.MatchString(comp) {
			if pathComponentRE.MatchString(strings.ToLower(comp)) {
				return Ref{}, fmt.Errorf("invalid repository path %q: path must be lowercase", path)
			}
			return Ref{}, fmt.Errorf("invalid repository path component %q: lowercase letters and digits, separated by single dots, underscores or dashes", comp)
		}
	}

	if host == "index.docker.io" {
		host = "docker.io"
	}
	if host == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return Ref{Host: host, Path: path, Tag: tag, Digest: digest}, nil
}
