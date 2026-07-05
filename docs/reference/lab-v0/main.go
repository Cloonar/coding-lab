package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	home := os.Getenv("HOME")
	defaultRoot := filepath.Join(home, "projects")
	defaultCfg := filepath.Join(home, ".claude.json")
	defaultState := filepath.Join(home, ".local", "state", "lab", "state.json")
	root := flag.String("root", defaultRoot, "projects root to scan")
	claudeCfg := flag.String("claude-config", defaultCfg, "path to claude.json — lab seeds folder-trust here before each spawn (MCP approval goes to the worktree's .claude/settings.local.json)")
	statePath := flag.String("state", defaultState, "path to lab's persistent state file (urls + last-opened timestamps)")
	tmuxBin := flag.String("tmux", "tmux", "path to tmux binary")
	prlimitBin := flag.String("prlimit", "prlimit", "path to prlimit binary (util-linux) used to bound each spawned session's RLIMIT_NOFILE")
	claudeBin := flag.String("claude", "claude", "path to claude binary")
	teaBin := flag.String("tea", "tea", "path to tea binary (Forgejo CLI for AFK-run issue claims)")
	gitBin := flag.String("git", "git", "path to git binary (AFK-run worktrees + Forgejo detection)")
	maxInstances := flag.Int("max-instances", defaultMaxInstances, "global cap on concurrent claude instances (login session excluded)")
	sessionNofile := flag.Int("session-nofile", 16384, "soft+hard RLIMIT_NOFILE pinned on every spawned session so one runaway agent hits its own EMFILE instead of exhausting the VM system-wide; 0 disables")
	flag.Parse()

	// The base spawn argv carries no --model/--effort: those are appended fresh per
	// spawn from the global setting (sessions.spawnConfig, wired below), so the UI's
	// model/effort selector governs every new session (#156). Unset, it resolves to
	// the documented opus[1m]/max defaults.
	sessions := NewSessions(*tmuxBin, []string{*claudeBin, "--remote-control", "%s", "--permission-mode", "auto"})
	// Bound each spawned session's descriptor budget (see Sessions.nofile). Set
	// here right after construction — like srv.maxInstances below — so the cap is
	// in force before the server handles its first spawn.
	sessions.prlimitBin = *prlimitBin
	sessions.nofile = *sessionNofile

	store := NewStore(*statePath)
	// Wire the spawn-config accessor: every spawn reads the persisted global
	// model + effort from the store at spawn time (#156). Lazy by design, so it is
	// safe to set before Load — the value is only read once a session is started,
	// by which point Load has populated the store.
	sessions.spawnConfig = store.SpawnConfig
	if err := store.Load(); err != nil {
		// Don't refuse to serve over a bad state file — log and start fresh.
		// Next mutation overwrites the corrupt file.
		log.Printf("load state %s: %v (starting with empty state)", *statePath, err)
	}
	// Drop URLs for sessions that aren't actually running anymore, so stale
	// links from a previous lab/microvm lifetime don't leak into the UI.
	live, err := sessions.List()
	if err != nil {
		log.Printf("tmux list-sessions during startup heal: %v", err)
	} else {
		liveSet := make(map[string]bool, len(live))
		for _, n := range live {
			liveSet[n] = true
		}
		if err := store.PruneDeadURLs(liveSet); err != nil {
			log.Printf("prune dead urls: %v", err)
		}
	}

	// claude auth status/login reuse the same binary as the remote-control
	// spawn. Login is global (one machine-level credential) and runs from $HOME;
	// --claudeai skips the subscription-vs-console picker.
	auth := NewAuth([]string{*claudeBin, "auth", "status", "--json"})
	loginArgv := []string{*claudeBin, "auth", "login", "--claudeai"}

	srv := NewServer(*root, *claudeCfg, sessions, store, auth, loginArgv, home)
	srv.maxInstances = *maxInstances
	// AFK-run seams: tea drives the issue claim, git owns the per-run worktree.
	// Worktrees live beside the state file, under <state-dir>/worktrees, which is
	// outside the projects scan root by construction.
	srv.tracker = NewTeaTracker(*teaBin)
	srv.git = NewGit(*gitBin)
	srv.worktreeRoot = filepath.Join(filepath.Dir(*statePath), "worktrees")

	// Re-capture deep links (from claude's session registry — see registry.go)
	// for sessions already running at startup (e.g. lab restarted while its
	// claude sessions kept running). PruneDeadURLs above dropped links for
	// sessions that died; this recovers them for the survivors, each showing
	// "connecting…" until its link lands.
	for _, n := range live {
		if n != loginSession && store.URL(n) == "" {
			srv.startCapture(n)
		}
	}

	// Reconcile leftover worktrees/branches from a previous lab lifetime before the
	// schedulers start (ADR-0017): guard-tear-down clean orphan worktrees (no live
	// session), keep dirty ones, and GC merged dangling lab//afk/ branches. Runs
	// synchronously here — re-adopted live sessions (above) are protected as owned,
	// and finishing before the reaper/scheduler goroutines means no Start can race it.
	srv.reconcileWorktrees()

	// Reap AFK runs in the background: claude --remote-control never self-exits,
	// so this watcher is what completes a run once its PR appears (or fails it on
	// death / budget overrun) and frees the slot. lab's first long-lived
	// server-side loop; in-flight runs are re-adopted from their session names
	// with the budget clock reset to now.
	go srv.watchAFKRuns()

	// Drive automatic AFK runs in the background: lab's second long-lived worker,
	// distinct from the reaper and the ~4s client poll. It launches runs for
	// auto-enabled projects under the global cap (#64 / ADR-0007), and like the
	// reaper no-ops when the AFK seams aren't wired.
	go srv.runAFKScheduler()

	log.Printf("lab listening on %s; scanning %s; state %s (max %d instances, nofile cap %d)", *addr, *root, *statePath, *maxInstances, *sessionNofile)
	if err := http.ListenAndServe(*addr, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}
