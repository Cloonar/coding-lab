package migrations

import (
	"io/fs"
	"regexp"
	"strconv"
	"testing"
)

// Migration filenames must be NNNN_name.sql — goose derives the version from
// the numeric prefix, so the prefix is the identity that must never collide.
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// versionsOf maps version → filename for one dialect tree, failing on any
// file that doesn't match the naming convention and — the regression this
// guards against — on two files claiming the same version (PRs #112 and #113
// both landed a 0008; goose refuses to init and the server won't start).
func versionsOf(t *testing.T, dialect string, tree fs.FS) map[int]string {
	t.Helper()
	entries, err := fs.ReadDir(tree, ".")
	if err != nil {
		t.Fatalf("%s: read dir: %v", dialect, err)
	}
	byVersion := make(map[int]string, len(entries))
	for _, e := range entries {
		name := e.Name()
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s: %s does not match NNNN_name.sql", dialect, name)
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if prev, ok := byVersion[v]; ok {
			t.Errorf("%s: duplicate migration version %d: %s and %s", dialect, v, prev, name)
			continue
		}
		byVersion[v] = name
	}
	return byVersion
}

// TestMigrationVersions asserts the invariants the goose provider needs at
// startup: unique versions per dialect, a gapless 1..N sequence (a gap means
// a botched renumber), and lockstep between the sqlite and postgres trees.
func TestMigrationVersions(t *testing.T) {
	sqlite := versionsOf(t, "sqlite", SQLite)
	postgres := versionsOf(t, "postgres", Postgres)

	if len(sqlite) == 0 {
		t.Fatal("sqlite: no migrations found")
	}
	for v := 1; v <= len(sqlite); v++ {
		if _, ok := sqlite[v]; !ok {
			t.Errorf("sqlite: gap in migration versions: missing %04d", v)
		}
	}

	for v, name := range sqlite {
		if _, ok := postgres[v]; !ok {
			t.Errorf("postgres: missing version %d (sqlite has %s)", v, name)
		}
	}
	for v, name := range postgres {
		if other, ok := sqlite[v]; !ok {
			t.Errorf("sqlite: missing version %d (postgres has %s)", v, name)
		} else if other != name {
			t.Errorf("version %d: dialect trees disagree on name: sqlite %s vs postgres %s", v, other, name)
		}
	}
}
