## Summary

<!-- What changed and why, in a sentence or two. -->

Closes #

## Checklist

- [ ] One commit per coherent change, following [Conventional Commits](https://www.conventionalcommits.org/) (`feat(web): …`, `fix(afk): …`, `refactor: …`).
- [ ] `nix flake check` passes locally, or the CI gates it maps to are green: the fast `native` check (every PR) and the path-gated `flake-check` (nix/Go-dependency changes) — see [`CONTRIBUTING.md`](https://github.com/Cloonar/coding-lab/blob/main/CONTRIBUTING.md#ci).
- [ ] Identifiers, UI copy, and docs use [`CONTEXT.md`](https://github.com/Cloonar/coding-lab/blob/main/CONTEXT.md) vocabulary verbatim; no parallel terms invented for concepts already in the glossary.
- [ ] Design decisions are recorded as an ADR in [`docs/adr/`](https://github.com/Cloonar/coding-lab/tree/main/docs/adr) where applicable.
- [ ] Behavior changes come with test changes.
- [ ] Fragile couplings (provider CLI behavior) update [`internal/compat/compat.md`](https://github.com/Cloonar/coding-lab/blob/main/internal/compat/compat.md) and its fixtures together, if touched.
