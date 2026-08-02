-- 0021_repo_imports — the repo_imports join table: which other repos a
-- repo's instances may read (sqlite dialect; issue #261).
--
-- repo_id is the importing (consumer) repo — the repo declaring that its
-- instances get read access to another repo's worktree — and cascades (ON
-- DELETE CASCADE): deleting a repo takes its own import declarations with
-- it, the same as every other per-repo child table (repo_secrets, labels,
-- ...).
--
-- target_repo_id deliberately carries NO delete action. A repo other repos
-- still import should not be deletable out from under them; the friendly,
-- named-importers refusal lives in store.DeleteRepo, which checks before
-- deleting and returns *ImportersError. The bare FK here is only a backstop
-- against a raced delete slipping past that check between the read and the
-- write — at that point a plain FOREIGN KEY violation is the correct, if
-- blunt, last line of defense.
--
-- No surrogate id column: repo_imports is a pure relation between two repos,
-- like issue_labels (0001_baseline) — the (repo_id, target_repo_id) pair IS
-- the identity, so it is the PRIMARY KEY directly rather than a column
-- nobody would ever look up by.
--
-- Self-import (repo_id = target_repo_id) is rejected app-side (and
-- defensively by the store, short of a full CHECK); an unknown
-- target_repo_id beyond the FK's own existence check is likewise app-side —
-- the same app-side-validation preference the schema already follows
-- elsewhere (repos.runner's enum, 0017) rather than duplicating logic the
-- API layer must enforce anyway for its error messages.
--
-- Identical to the postgres dialect: no column-type divergence to call out.

-- +goose Up
CREATE TABLE repo_imports (
    repo_id        TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target_repo_id TEXT NOT NULL REFERENCES repos (id),
    PRIMARY KEY (repo_id, target_repo_id)
);

-- +goose Down
DROP TABLE repo_imports;
