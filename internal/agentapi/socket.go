package agentapi

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// SocketPath returns the agent-API unix socket path inside stateDir.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, "agent.sock")
}

// ListenSocket listens on a unix domain socket at path, removing any stale
// socket file left behind by a previous crash (a clean shutdown unlinks it —
// Go's *net.UnixListener unlinks on Close). The socket is chmod'd 0700 so only
// the service user can connect; the state dir itself is 0700, which covers the
// window between Listen and Chmod.
func ListenSocket(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
