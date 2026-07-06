package store

// Typed errors shared by the credential and repo accessors, plus the
// driver-specific constraint-violation detection they need. The store maps
// UNIQUE and FOREIGN KEY violations to sentinels so the API layer can answer
// 4xx without parsing driver error strings.

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite "modernc.org/sqlite"
)

// ErrNameTaken is the sentinel for a UNIQUE-name violation (credentials.name,
// repos.name).
var ErrNameTaken = errors.New("name already taken")

// ErrReferenced is the sentinel matched by errors.Is when a credential delete
// is refused because repos still reference it via either FK column
// (design §12: delete blocked while referenced).
var ErrReferenced = errors.New("credential is referenced")

// ErrCredentialGone is the sentinel for a FOREIGN KEY violation on
// repos.credential_id / repos.forge_credential_id: the referenced credential
// was deleted between the caller's kind check and the write (the reverse of
// the ErrReferenced race — DeleteCredential's ref-count saw zero because the
// repo write had not landed yet). The API answers 409.
var ErrCredentialGone = errors.New("credential no longer exists")

// ReferencedError is the concrete error DeleteCredential returns when refused:
// it carries the number of referencing repos for the API's 409 body.
// errors.Is(err, ErrReferenced) matches it; errors.As recovers the count.
type ReferencedError struct {
	Repos int // repos referencing the credential (a repo using it in both FK columns counts once)
}

func (e *ReferencedError) Error() string {
	noun := "repositories"
	if e.Repos == 1 {
		noun = "repository"
	}
	return fmt.Sprintf("credential is referenced by %d %s", e.Repos, noun)
}

// Is makes errors.Is(err, ErrReferenced) true for any ReferencedError.
func (e *ReferencedError) Is(target error) bool { return target == ErrReferenced }

// SQLite extended result codes (modernc.org/sqlite/lib values, inlined so the
// generated lib package is not imported for three constants).
const (
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// isUniqueViolation reports whether err is a UNIQUE (or PK) constraint
// violation on either backend.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" // unique_violation
	}
	var sqErr *sqlite.Error
	if errors.As(err, &sqErr) {
		return sqErr.Code() == sqliteConstraintUnique || sqErr.Code() == sqliteConstraintPrimaryKey
	}
	return false
}

// isForeignKeyViolation reports whether err is a FOREIGN KEY constraint
// violation on either backend.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503" // foreign_key_violation
	}
	var sqErr *sqlite.Error
	if errors.As(err, &sqErr) {
		return sqErr.Code() == sqliteConstraintForeignKey
	}
	return false
}
