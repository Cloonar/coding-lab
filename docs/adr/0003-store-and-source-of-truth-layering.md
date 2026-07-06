# One store layer over SQLite and Postgres; the DB is never the only witness

Storage is SQLite by default and PostgreSQL by DSN, behind one repository layer on plain `database/sql` — `modernc.org/sqlite` and `pgx`'s stdlib driver, both CGO-free — with goose migrations embedded in the binary and applied on startup (D6). Queries are written once in a portable SQL subset with `?` placeholders; the store rebinds to `$N` on postgres. Migrations live in two dialect-native directories, `migrations/sqlite/` and `migrations/postgres/`, kept honest by a parity test that walks both embedded trees and asserts identical version-number sets on every `go test`.

The concrete conventions the layer pins:

- **Timestamps** are TEXT, fixed-width, always UTC: `2006-01-02T15:04:05.000Z07:00` (exactly three fractional digits, `Z`). Fixed width is load-bearing — every `ORDER BY` and range comparison on a timestamp column relies on lexicographic order equaling chronological order. One helper pair (`fmtTime`/`parseTime`) in the store; nothing formats timestamps ad hoc.
- **IDs** are app-generated TEXT PKs, `<prefix>_<32 lowercase hex>` (16 random bytes) — portable across dialects, no autoincrement coupling, greppable by prefix. The one exception: `web_sessions.id` is the sha256 hex of the cookie token, because it *is* the lookup key.
- **SQLite open recipe** (pinned): DSN `file:<state>/lab.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate` and `db.SetMaxOpenConns(1)` — single-writer discipline; lab's traffic is tiny. `foreign_keys(1)` is mandatory: without it every `ON DELETE CASCADE` silently no-ops on sqlite. Postgres uses the default pool.

The second half of the decision is what the DB is *for*. Source-of-truth layering (D6, from the bug class reference ADR-0013 documents): **tmux** answers "is the session alive", **git refs** answer "what is claimed / what work exists", **the tracker** answers "is there a PR / is the issue open" — the DB stores configuration, credentials, the built-in tracker, and run *history*. Reconciliation (startup + throttled sweep) re-derives live state from those sources; the DB must never be the only witness to something the world can contradict. v0's label-flap bug existed precisely because one fact (the claim) had two records in a place two actors wrote; a DB row asserting "this run is live" is the same trap in new clothes.

## Status

Accepted. Implements D6 with the concrete choices from the build design (§2, §3).

## Considered options

- **CGO sqlite (`mattn/go-sqlite3`).** Rejected: the single static binary is the deployment shape (D1); `modernc.org/sqlite` keeps `CGO_ENABLED=0` everywhere.
- **ORM or sqlc.** Rejected: a thin repository layer over `database/sql` keeps the portable subset visible in one place and the dependency list minimal; the schema is small and stable.
- **One migration directory with dialect conditionals.** Rejected: conditional SQL in one file is harder to read and easier to break than two dialect-native trees; the parity test makes divergence a test failure instead of a runtime surprise.
- **Epoch-integer timestamps.** Rejected: fixed-width TEXT is readable in every SQL shell, identical across dialects, and lexicographically ordered — the property the fixed width guarantees and the helper pair enforces.
- **DB-generated IDs (AUTOINCREMENT/sequences).** Rejected: app-generated IDs behave identically on both backends and are known before the INSERT. The only DB sequences kept are the per-repo issue/CR counters, incremented inside the insert transaction.
- **DB as liveness store** (runs/claims authoritative in SQL). Rejected: the label-flap class — two records of one fact drift and fight. Liveness stays derived; `runs.outcome='active'` is reconciled against live tmux at startup, adopted or marked dead, never trusted.

## Consequences

- The migration parity test lands with migration 0001 and runs on every `go test`; the postgres store suite runs whenever `LAB_TEST_POSTGRES_DSN` is set (CI provides a service container from M2).
- Store code never branches on dialect outside open/rebind; a query that needs dialect-specific SQL is a design smell to fix in the query.
- Restart behavior falls out of the layering: budgets persist on the run row (D12b) because history is the DB's business, while "is it alive" is re-asked of tmux every time.
- Deleting a repo cascades through the built-in tracker (a store test asserts it — the sqlite pragma makes it real).
