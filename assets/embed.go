// Package assets holds the vendored bundles compiled into the lab binary.
package assets

import "embed"

// Skills is the vendored skills bundle (brief D13): mattpocock/skills at the
// pinned rev plus the local tdd patch — provenance and bump procedure in
// skills/README.md. The seeder copies it verbatim into
// <worktree>/.claude/skills/ on every spawn; the bundle content itself is
// never modified by code (a configurable bundle source is roadmap).
//
//go:embed all:skills
var Skills embed.FS
