package imageref

import (
	"strings"
	"testing"
)

// digestA is a grammatically valid digest for parse tests; the value is
// arbitrary — Parse never checks it against any content.
var digestA = "sha256:" + strings.Repeat("0123456789abcdef", 4)

func TestParse(t *testing.T) {
	longestTag := strings.Repeat("t", 128)
	cases := []struct {
		name string
		in   string
		want Ref // ignored when wantErr is set
		// wantErr is a substring the error message must contain — these
		// strings are user-facing, so the tests pin their actionable core.
		wantErr string
	}{
		// Fully qualified happy paths.
		{name: "host and multi-segment path", in: "ghcr.io/foo/bar:v1", want: Ref{Host: "ghcr.io", Path: "foo/bar", Tag: "v1"}},
		{name: "deep path", in: "example.com:443/a/b/c:1.0", want: Ref{Host: "example.com:443", Path: "a/b/c", Tag: "1.0"}},
		{name: "localhost is a host", in: "localhost/foo", want: Ref{Host: "localhost", Path: "foo", Tag: "latest"}},
		{name: "localhost with port", in: "localhost:5000/foo/bar:v1", want: Ref{Host: "localhost:5000", Path: "foo/bar", Tag: "v1"}},
		{name: "ip host with port", in: "127.0.0.1:5000/repo:v1", want: Ref{Host: "127.0.0.1:5000", Path: "repo", Tag: "v1"}},

		// docker.io library shorthand and index.docker.io normalization.
		{name: "docker.io single segment gains library", in: "docker.io/debian", want: Ref{Host: "docker.io", Path: "library/debian", Tag: "latest"}},
		{name: "docker.io single segment with tag", in: "docker.io/debian:bookworm", want: Ref{Host: "docker.io", Path: "library/debian", Tag: "bookworm"}},
		{name: "docker.io explicit library untouched", in: "docker.io/library/debian:bookworm", want: Ref{Host: "docker.io", Path: "library/debian", Tag: "bookworm"}},
		{name: "docker.io org path untouched", in: "docker.io/grafana/grafana", want: Ref{Host: "docker.io", Path: "grafana/grafana", Tag: "latest"}},
		{name: "index.docker.io normalizes", in: "index.docker.io/debian:bookworm", want: Ref{Host: "docker.io", Path: "library/debian", Tag: "bookworm"}},
		{name: "index.docker.io with library", in: "index.docker.io/library/debian", want: Ref{Host: "docker.io", Path: "library/debian", Tag: "latest"}},

		// Tag defaulting, digest-only, tag+digest.
		{name: "no tag no digest defaults latest", in: "ghcr.io/foo/bar", want: Ref{Host: "ghcr.io", Path: "foo/bar", Tag: "latest"}},
		{name: "digest only keeps tag empty", in: "ghcr.io/foo/bar@" + digestA, want: Ref{Host: "ghcr.io", Path: "foo/bar", Digest: digestA}},
		{name: "tag and digest", in: "ghcr.io/foo/bar:v1@" + digestA, want: Ref{Host: "ghcr.io", Path: "foo/bar", Tag: "v1", Digest: digestA}},
		{name: "docker.io shorthand with digest", in: "docker.io/debian@" + digestA, want: Ref{Host: "docker.io", Path: "library/debian", Digest: digestA}},

		// Path grammar fine points.
		{name: "separators in components", in: "example.com/a-b/c__d/e.f/g--h:v1", want: Ref{Host: "example.com", Path: "a-b/c__d/e.f/g--h", Tag: "v1"}},
		{name: "tag with underscores and dots", in: "example.com/foo:1.2_3-rc", want: Ref{Host: "example.com", Path: "foo", Tag: "1.2_3-rc"}},
		{name: "tag at max length", in: "example.com/foo:" + longestTag, want: Ref{Host: "example.com", Path: "foo", Tag: longestTag}},

		// Rejections: unqualified refs.
		{name: "empty", in: "", wantErr: "image ref is empty"},
		{name: "bare name", in: "debian", wantErr: "fully qualified"},
		{name: "bare name with tag", in: "debian:bookworm", wantErr: "fully qualified"},
		{name: "namespace without host", in: "library/debian", wantErr: "fully qualified"},
		{name: "scheme prefix", in: "https://docker.io/library/debian", wantErr: "must not include a scheme"},

		// Rejections: host.
		{name: "host with bad leading char", in: "-foo.com/bar", wantErr: "invalid registry host"},
		{name: "host with non-numeric port", in: "foo.com:port/bar", wantErr: "invalid registry host"},

		// Rejections: path.
		{name: "missing path", in: "docker.io/", wantErr: "no repository path"},
		{name: "empty path component", in: "example.com/foo//bar", wantErr: "empty path component"},
		{name: "uppercase path", in: "docker.io/Library/debian", wantErr: "must be lowercase"},
		{name: "leading separator in component", in: "example.com/_foo", wantErr: "invalid repository path component"},
		{name: "trailing separator in component", in: "example.com/foo-", wantErr: "invalid repository path component"},
		{name: "triple underscore", in: "example.com/a___b", wantErr: "invalid repository path component"},
		{name: "double dot", in: "example.com/a..b", wantErr: "invalid repository path component"},

		// Rejections: tag.
		{name: "empty tag", in: "docker.io/debian:", wantErr: "empty tag"},
		{name: "tag starting with dash", in: "docker.io/debian:-v1", wantErr: "invalid tag"},
		{name: "tag starting with dot", in: "docker.io/debian:.v1", wantErr: "invalid tag"},
		{name: "tag too long", in: "example.com/foo:" + longestTag + "x", wantErr: "invalid tag"},
		{name: "tag with slash", in: "example.com/foo:v/1", wantErr: "invalid tag"},

		// Rejections: digest.
		{name: "empty digest", in: "docker.io/debian@", wantErr: "empty digest"},
		{name: "short digest", in: "docker.io/debian@sha256:abc", wantErr: "invalid digest"},
		{name: "uppercase digest hex", in: "docker.io/debian@sha256:" + strings.Repeat("A", 64), wantErr: "invalid digest"},
		{name: "wrong algorithm", in: "docker.io/debian@md5:" + strings.Repeat("a", 64), wantErr: "invalid digest"},
		{name: "double at", in: "example.com/a@b@" + digestA, wantErr: "invalid digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse(%q) error = %q, want it to contain %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestStringCanonical(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"docker.io/debian", "docker.io/library/debian:latest"},
		{"index.docker.io/debian:bookworm", "docker.io/library/debian:bookworm"},
		{"ghcr.io/foo/bar", "ghcr.io/foo/bar:latest"},
		{"ghcr.io/foo/bar:v1", "ghcr.io/foo/bar:v1"},
		{"ghcr.io/foo/bar@" + digestA, "ghcr.io/foo/bar@" + digestA},
		{"ghcr.io/foo/bar:v1@" + digestA, "ghcr.io/foo/bar:v1@" + digestA},
		{"localhost:5000/foo", "localhost:5000/foo:latest"},
	}
	for _, tc := range cases {
		r, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", tc.in, err)
		}
		if got := r.String(); got != tc.want {
			t.Errorf("Parse(%q).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Canonical strings must be fixed points: parsing one back yields the same
// Ref and the same string, so a stored ref never drifts on re-save.
func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{
		"docker.io/debian",
		"index.docker.io/debian:bookworm",
		"ghcr.io/foo/bar:v1",
		"ghcr.io/foo/bar@" + digestA,
		"ghcr.io/foo/bar:v1@" + digestA,
		"localhost:5000/a-b/c__d:1.0",
	} {
		r1, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", in, err)
		}
		r2, err := Parse(r1.String())
		if err != nil {
			t.Fatalf("Parse(%q) (canonical of %q) unexpected error: %v", r1.String(), in, err)
		}
		if r1 != r2 {
			t.Errorf("round-trip of %q: %+v != %+v", in, r1, r2)
		}
		if r1.String() != r2.String() {
			t.Errorf("round-trip of %q: string %q != %q", in, r1.String(), r2.String())
		}
	}
}

func TestPinned(t *testing.T) {
	if (Ref{Host: "h.io", Path: "p", Tag: "v1"}).Pinned() {
		t.Error("ref without digest reports Pinned")
	}
	if !(Ref{Host: "h.io", Path: "p", Digest: digestA}).Pinned() {
		t.Error("ref with digest reports not Pinned")
	}
}
