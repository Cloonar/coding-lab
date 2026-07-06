package instance

import (
	"errors"
	"fmt"
)

// Sentinels the API layer maps onto the pinned instance-API status codes
// (POST /api/v1/repos/{id}/instances → 201 | 409 | 400).
var (
	// ErrOverCap: the live-instance cap (settings max_instances, or the repo's
	// max_instances_override) is already reached. API → 409.
	ErrOverCap = errors.New("instance cap reached")
	// ErrLoggedOut: the forced pre-spawn auth refresh found the provider logged
	// out — a remote-control session spawned now would hit the login wall
	// immediately. API → 409.
	ErrLoggedOut = errors.New("provider is logged out")
	// ErrRepoNotReady: the repo's clone has not completed (clone_status !=
	// ready), so there is no bare reference to fork a worktree from. API → 409.
	ErrRepoNotReady = errors.New("repository is not ready")
	// ErrAFKStopUnsupported: Stop of a session whose run is an AFK kind is the
	// AFK engine's job (M5). In M3 it is refused. API → 501.
	ErrAFKStopUnsupported = errors.New("stopping AFK instances is not supported yet")
)

// BadRequestError marks invalid operator input (an unknown model/effort). The
// API answers 400 with the message.
type BadRequestError struct{ msg string }

func (e *BadRequestError) Error() string { return e.msg }

func badRequestf(format string, args ...any) *BadRequestError {
	return &BadRequestError{msg: fmt.Sprintf(format, args...)}
}

// StartFailedError wraps an infrastructure failure of the synchronous Start
// sequence (worktree creation, seeding, spawn) AFTER the pre-flight checks
// passed. The git/spawn cause surfaces verbatim in the API error (v0
// property); the API answers 500 with the cause message rather than an opaque
// "internal error" so the operator's banner shows what git said.
type StartFailedError struct{ cause error }

func (e *StartFailedError) Error() string { return e.cause.Error() }
func (e *StartFailedError) Unwrap() error { return e.cause }
