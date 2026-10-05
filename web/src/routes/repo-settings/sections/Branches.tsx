// Branches section (issue #61): the default branch and the two branch naming
// rules — a thin renderer over the form store. Every control edits a draft;
// the page's save bar sends the changed ones, trimmed (form.tsx, fields.ts).
// The browser checks them on Save: none may be empty, and the AFK pattern
// must contain <N> exactly once.

import { TextField } from '../Field';

export default function BranchesSection() {
  return (
    <div class="card settings-card">
      <TextField name="default_branch" required />
      <TextField
        name="afk_branch_pattern"
        mono
        required
        hint="<N> stands for the issue number, for example afk/<N> or issue-<N>. Letters, digits, . _ / - only; it may not overlap the manual prefix."
      />
      <TextField
        name="manual_branch_prefix"
        mono
        required
        hint="Literal prefix for the branches of runs you start yourself, for example lab/ or wip/."
      />
    </div>
  );
}
