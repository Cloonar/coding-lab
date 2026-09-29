-- 0023_repo_ssh_targets — the repo_ssh_targets cache table: which Warpgate
-- SSH targets currently carry a repo's role (sqlite dialect; issue #39,
-- Warpgate bastion integration).
--
-- This is a CACHE, not a lab-side grant model. Warpgate stays the single
-- source of truth for "repo R may reach SSH target T": a lab repo gets one
-- Warpgate role, and R may reach T iff T carries that role in Warpgate today.
-- Lab never invents its own access decision here — it only remembers, in
-- this table, the last Warpgate answer it saw for a repo's role, so the
-- table can be stale between syncs but is never authoritative.
--
-- Why it exists at all: the spawn path must decide WITHOUT calling Warpgate
-- whether a repo has any SSH grants, because the fail-closed rule only
-- applies to a repo with >=1 assigned target — a grant-free repo must never
-- block a spawn on an unreachable Warpgate, and never even attempt the call.
-- That decision (does this repo have any targets at all?) has to be
-- decidable locally, from data lab already holds, or the two halves of the
-- rule collapse into each other: a down Warpgate would either fail closed
-- for every repo (grant-free ones included) or fail open for every repo
-- (defeating the point of the bastion). This table is that local decision
-- surface.
--
-- Who writes it, and when — three paths, the same ON DELETE CASCADE shape,
-- writing by two different granularities:
--   * the per-repo picker's toggle (one target on/off at a time) does a
--     single-row upsert/delete as the operator clicks — see
--     AddRepoSSHTarget/RemoveRepoSSHTarget in internal/store/repossh.go;
--   * the picker's own listing, a startup heal pass, and a grant-bearing
--     spawn's pre-flight check each REPLACE the repo's whole cached set
--     wholesale from a fresh read of Warpgate's truth — see
--     ReplaceRepoSSHTargets in the same file — so a toggle missed while lab
--     was down, or a target renamed/removed directly in Warpgate, self-heals
--     on the next full read rather than drifting forever.
--
-- target_id is Warpgate's target UUID, opaque to lab — never parsed,
-- never assumed stable in shape beyond "the identifier Warpgate uses for
-- this target." target_name is Warpgate's target name AS OF THE LAST SYNC:
-- both a display label and the ssh alias, kept here (rather than re-fetched
-- live) so the picker and any status view can render without another
-- Warpgate round trip; it can go stale between syncs like the rest of this
-- cache, and self-heals the same way.
--
-- No surrogate id column: like repo_imports (0021), the pair IS the
-- identity — (repo_id, target_id) is the PRIMARY KEY directly rather than a
-- column nobody would look up by.
--
-- repo_id cascades (ON DELETE CASCADE): deleting a repo takes its cached SSH
-- grants with it, the same as every other per-repo child table (repo_secrets,
-- labels, repo_imports, ...). There is no FK to a Warpgate table for
-- target_id — Warpgate is a separate system lab does not own a schema for.
--
-- Identical to the postgres dialect: no column-type divergence to call out.

-- +goose Up
CREATE TABLE repo_ssh_targets (
    repo_id     TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_id   TEXT NOT NULL,   -- Warpgate's target UUID, opaque to lab
    target_name TEXT NOT NULL,   -- Warpgate's target name at last sync (display + ssh alias)
    PRIMARY KEY (repo_id, target_id)
);

-- +goose Down
DROP TABLE repo_ssh_targets;
