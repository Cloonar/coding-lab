package main

// Dropping the ambient capability set once lab's privileged binds are done
// (issue #106).
//
// The NixOS module grants the unit AmbientCapabilities=CAP_NET_BIND_SERVICE
// when a preview listener sits on a port below 1024 (the defaults are :443
// and :80), because that is the only way an unprivileged service user can
// bind one. Ambient capabilities are the one capability set that survives
// execve into an ordinary, non-setuid, no-file-caps binary — that is what
// they are for — so left alone every process lab starts would inherit the
// right to bind privileged ports: the tmux server, and through it every
// agent session and every dev server an agent runs. An agent squatting on
// :443 or :80 of the host is exactly what the grant must not buy.
//
// So run() binds both preview listeners first thing, before anything that
// starts a child process, and then clears the ambient set. Lab's own
// permitted/effective sets keep the capability (clearing the ambient set
// does not touch them, and lab binds its other listeners later); only
// processes exec'd from here on start without it.
//
// The Linux mechanics live in ambientcaps_linux.go; other platforms have no
// ambient capabilities and get a no-op (ambientcaps_other.go). parseCapAmb is
// here, untagged, so its test runs everywhere.

import (
	"bufio"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// parseCapAmb returns the CapAmb mask from the text of a /proc/<pid>/status
// file: one hex field, e.g. "CapAmb:\t0000000000000400" for
// CAP_NET_BIND_SERVICE (bit 10). A status without the line — a kernel older
// than 4.3, which has no ambient set at all — is 0, not an error: there is
// nothing to drop.
func parseCapAmb(status string) (uint64, error) {
	sc := bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "CapAmb:")
		if !ok {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing CapAmb %q: %w", strings.TrimSpace(v), err)
		}
		return mask, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}

// dropAmbientCapabilities clears the process's ambient capability set and
// logs the outcome: info when it cleared something, a warning when it could
// not. Never fatal — lab still works, it only means children inherit what
// the unit granted, which is the operator's to know rather than a reason to
// refuse to start. Called unconditionally, previews on or off: it costs one
// small file read, and nothing lab starts ever needs an ambient capability.
func dropAmbientCapabilities(logger *slog.Logger) {
	cleared, err := clearAmbientCaps()
	switch {
	case err != nil:
		logger.Warn("could not drop the ambient capability set; child processes (tmux, agent sessions) inherit it",
			"component", "main", "cap_amb", fmt.Sprintf("%016x", cleared), "err", err)
	case cleared != 0:
		logger.Info("dropped the ambient capability set after binding the preview listeners; child processes start without it",
			"component", "main", "cap_amb", fmt.Sprintf("%016x", cleared))
	}
}
