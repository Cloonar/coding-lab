package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// Auth answers one question: is this machine logged in to Claude? It shells out
// to `claude auth status --json` and reads the .loggedIn field. The argv is
// injectable so tests can substitute a stand-in command without a real claude
// binary or Anthropic credentials. Holds no state — the freshness cache lives
// on Server, since the cache is a render concern, not a property of the check.
//
// Peeking at ~/.claude/.credentials.json directly was rejected: it couples lab
// to claude's internal storage and skips token-expiry validation, which the
// status command does for us.
type Auth struct {
	// statusArgv is the command whose stdout is parsed as {"loggedIn": bool}.
	statusArgv []string
}

func NewAuth(statusArgv []string) *Auth {
	return &Auth{statusArgv: statusArgv}
}

// LoggedIn runs the status command and reports whether the machine is logged
// in. Stdout is parsed regardless of exit code: `claude auth status` may exit
// non-zero when logged out while still emitting {"loggedIn": false}, and that
// is a definitive answer, not an error. A genuine failure (no parseable JSON)
// is returned so the caller can log it; the caller treats that as logged-out.
func (a *Auth) LoggedIn() (bool, error) {
	out, runErr := exec.Command(a.statusArgv[0], a.statusArgv[1:]...).Output()
	loggedIn, parseErr := parseLoggedIn(out)
	if parseErr == nil {
		return loggedIn, nil
	}
	if runErr != nil {
		return false, fmt.Errorf("claude auth status: %w", runErr)
	}
	return false, fmt.Errorf("parse auth status: %w", parseErr)
}

func parseLoggedIn(data []byte) (bool, error) {
	var s struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return false, err
	}
	return s.LoggedIn, nil
}
