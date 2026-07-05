# Lab is a shared module in `utils/modules/`, imported per host

`lab`'s source, derivation, and systemd module live at `utils/modules/lab/` as one self-contained directory, imported explicitly (`./utils/modules/lab`) by the hosts that run it — currently `dev` and `dev-new`.

Supersedes [ADR-0004](0004-lab-vendored-under-dev-microvm.md). That ADR vendored `lab` under the dev guest because it was single-host by design, and it named the exit condition: *"If `lab` ever grows to be useful on another machine, migration is `git mv`."* `dev-new` (the Framework 13 standalone board) runs the same lab stack, so the single-host premise is gone — and the first attempt at a second host copied all ~11k lines into `hosts/dev-new/modules/lab/`, which is exactly the drift trap a shared module exists to prevent.

ADR-0004's second concern — everything under `utils/` triggers the all-host pre-commit dry-build — is real and **accepted**, not solved. Two hosts sharing one copy beats per-host copies whose dry-builds were cheaper but whose contents would silently diverge.

## Considered options

- **Keep one copy per host** (`hosts/dev/modules/lab/` + `hosts/dev-new/modules/lab/`). Rejected: 11k duplicated lines, every fix must land twice, and nothing detects when it doesn't.
- **`utils/pkgs/lab/` + overlay entry + module in `utils/modules/`** (the conventional package shape). Rejected, same as in ADR-0004: the overlay surfaces the package to every host's package set for a tool only two hosts run. The self-contained module keeps `lab` invisible to hosts that don't import it.
- **Special-case `utils/modules/lab/` in `scripts/pre-commit`** to dry-build only its importers. Still rejected (ADR-0004, ADR-0003): hardcoded coupling in an otherwise generic script. Revisit only if lab-edit commit times become unworkable in practice.

## Consequences

- Every `lab` edit now dry-builds the whole fleet at commit time (the `shared` regex matches `^utils/`), not just one host. This is the price of the move; `git commit --no-verify` remains the manual escape hatch for pure-HTML iteration.
- Hosts opt in by importing `./utils/modules/lab` — nothing is surfaced globally. A third lab host is a one-line import.
- ADR-0004's pattern advice still holds for genuinely single-host tools: vendor them under `hosts/<host>/modules/<tool>/` and migrate with `git mv` when a second host appears.
