package codex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// SeedWorkspace implements provider.AgentProvider: pre-approve the worktree
// so codex launches unattended. It writes NOTHING inside the worktree — both
// grants are guarded appends to codex's HOME-GLOBAL state under the run's
// private instance HOME opts.Home (atomic tmp+rename), never the machine's
// master store (issue #202):
//
//  1. Directory trust in <opts.Home>/.codex/config.toml (SeedTrust). The argv
//     route (`-c projects...trust_level`) is DEAD on 0.133 — live-verified: the
//     first-run trust prompt still appears — so the working mechanism is a
//     config append. Live-validated 2026-07-10: an appended
//     [projects."<dir>"] table suppressed the trust prompt on a real spawn.
//  2. The AGENTS.local.md bridge in the global <opts.Home>/.codex/AGENTS.md
//     (SeedAgentsBridge). codex's project_doc_fallback_filenames override is
//     fallback-only per directory level: a repo-committed AGENTS.md silently
//     skips it (live-verified), but the global $CODEX_HOME/AGENTS.md is ALWAYS
//     concatenated — so lab maintains a marker-guarded block there pointing
//     the agent at the workspace-root AGENTS.local.md.
//
// Both grants are HOME-global, so an empty opts.Home skips them entirely (a run
// with no per-run home gets no global write, never a fallback to the master
// store). opts.Incogni is a no-op: codex 0.133 writes no commit/PR attribution
// at the source (the codex_git_commit feature is off; `commit_attribution` is
// an unknown config key), so there is nothing to disable — the declared
// ScrubPatterns stay as the defensive backstop (ADR-0033).
//
// The lab-side seeder covers the worktree files (context file, skills,
// .git/info/exclude) from SeedMeta; nothing here touches git state.
func (p *Provider) SeedWorkspace(worktree string, opts provider.SeedOpts) error {
	if opts.Home == "" {
		return nil // no per-run home ⇒ no HOME-global grants (never the master store)
	}
	codexHome := instanceCodexHome(opts.Home)
	if err := SeedTrust(filepath.Join(codexHome, "config.toml"), worktree); err != nil {
		return err
	}
	if err := SeedAgentsBridge(filepath.Join(codexHome, "AGENTS.md")); err != nil {
		return err
	}
	return nil
}

// SeedTrust appends a trusted [projects."<dir>"] table to codex's global
// config.toml unless the table header is already present — whatever its
// trust_level. codex auto-writes trust entries for directories it has run
// in, and it rewrites this file itself, so the guard must tolerate codex
// mutating it between seedings: the check is plain string containment on the
// exact header line, and a hit means HANDS OFF — never rewrite or reorder
// existing content, append-only. Creates the file (0o644, parent dirs) when
// missing. Exported for the compat trust test.
func SeedTrust(configPath, dir string) error {
	header := projectsHeader(dir)
	data, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	if containsLine(data, header) {
		return nil // codex or an earlier seeding already owns this entry
	}
	var buf strings.Builder
	buf.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString("\n" + header + "\ntrust_level = \"trusted\"\n")
	return writeAtomic(configPath, []byte(buf.String()))
}

// projectsHeader renders the exact [projects."<dir>"] table header codex
// writes, TOML-escaping backslashes and double quotes in the path.
func projectsHeader(dir string) string {
	escaped := strings.ReplaceAll(dir, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `[projects."` + escaped + `"]`
}

// containsLine reports whether any line of data equals want after trimming
// surrounding whitespace — the exact-header-line containment guard.
func containsLine(data []byte, want string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// agentsBridgeMarker guards the bridge block: its presence anywhere in the
// global AGENTS.md means the bridge is installed and the file is left alone.
const agentsBridgeMarker = "<!-- lab:agents-local-bridge -->"

// agentsBridgeBlock is the marker-guarded block SeedAgentsBridge maintains
// in the global AGENTS.md (marker first; keep this exact shape).
const agentsBridgeBlock = agentsBridgeMarker + "\n" +
	"If a file named `AGENTS.local.md` exists at the workspace root, read it and\n" +
	"follow its instructions — it carries this workspace's session context.\n"

// SeedAgentsBridge installs the AGENTS.local.md bridge block into the global
// AGENTS.md at agentsFile: created containing the block when the file is
// missing, appended when present but unmarked, left untouched when the
// marker is already there. Append-only — an operator's own global
// instructions above the block always survive. Exported for the compat test.
func SeedAgentsBridge(agentsFile string) error {
	data, err := os.ReadFile(agentsFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", agentsFile, err)
	}
	if strings.Contains(string(data), agentsBridgeMarker) {
		return nil // bridge already installed
	}
	var buf strings.Builder
	buf.Write(data)
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString(agentsBridgeBlock)
	return writeAtomic(agentsFile, []byte(buf.String()))
}

// writeAtomic writes data to path via tmpfile + fsync + rename (claudecode's
// pinned pattern), creating parent directories and normalizing the file mode
// to 0o644 — both seeded files are world-readable config, not credentials.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("tmpfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
