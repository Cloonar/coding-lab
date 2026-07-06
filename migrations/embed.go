// Package migrations embeds the goose SQL migrations for both supported
// database dialects. The two trees must stay in lockstep: a parity test
// asserts identical version-number sets on every `go test`.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed all:sqlite all:postgres
var root embed.FS

// SQLite and Postgres are the embedded per-dialect migration trees, with the
// NNNN_*.sql files at their root — the shape goose's provider expects.
var (
	SQLite   fs.FS = mustSub("sqlite")
	Postgres fs.FS = mustSub("postgres")
)

func mustSub(dir string) fs.FS {
	sub, err := fs.Sub(root, dir)
	if err != nil {
		panic("migrations: sub " + dir + ": " + err.Error())
	}
	return sub
}
