package agentapi

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// SocketDir returns <state>/agent — the directory holding the agent-API unix
// socket, and the exact unit the container runner bind-mounts into a session
// (podmanx.RunSpec.AgentDir, issue #205). The socket lives in its OWN
// directory because a containerized session must keep reaching the API across
// a server restart: bind-mounting the socket FILE would pin the dead inode a
// restart leaves behind (the new server's Listen creates a NEW inode at the
// same path, invisible through the old mount), while mounting the DIRECTORY
// lets the restarted server re-create the socket inside it and a container
// that outlives the restart sees the fresh inode at the unchanged path.
func SocketDir(stateDir string) string {
	return filepath.Join(stateDir, "agent")
}

// SocketPath returns the agent-API unix socket path inside stateDir:
// <state>/agent/agent.sock (moved from <state>/agent.sock for the container
// runner — see SocketDir; LegacySocketSymlink keeps the old path serving
// pre-upgrade sessions).
func SocketPath(stateDir string) string {
	return filepath.Join(SocketDir(stateDir), "agent.sock")
}

// ListenSocket listens on a unix domain socket at path, creating the socket's
// parent directory (0700 — tightened even when it pre-exists looser, like the
// state dir itself) and removing any stale socket file left behind by a
// previous crash (a clean shutdown unlinks it — Go's *net.UnixListener
// unlinks on Close). The socket is chmod'd 0700 so only the service user can
// connect; dir and state dir are 0700, which covers the window between Listen
// and Chmod. 0700 is container-compatible by construction: the runner spawns
// with --userns=keep-id, so the agent inside is the same uid as the server.
func ListenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
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

// LegacySocketSymlink installs the back-compat symlink <state>/agent.sock →
// agent/agent.sock at the pre-#205 socket path. Sessions spawned by an older
// server carry LAB_URL=unix://<state>/agent.sock, and those sessions outlive
// the upgrade (tmux survives a lab restart); without the link their labctl
// would dial a path that no longer exists. Whatever a previous binary left at
// the old path — the real socket, a stale file, or an earlier symlink — is
// removed first; the target is RELATIVE (agent/agent.sock) so the link stays
// valid however the state dir is reached. Callers treat a failure as a
// warning, never fatal: only pre-upgrade sessions depend on this path, and a
// fresh deployment has none.
func LegacySocketSymlink(stateDir string) error {
	link := filepath.Join(stateDir, "agent.sock")
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Symlink(filepath.Join("agent", "agent.sock"), link)
}
