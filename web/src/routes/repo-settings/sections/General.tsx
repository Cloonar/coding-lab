// General section (issue #61): the repo's name, its git author identity and
// Incogni — a thin renderer over the form store. Every control edits a draft;
// the page's save bar sends the changed ones (form.tsx, fields.ts). The git
// author fields are overridable: each says "inherited" or "set here" and
// shows what it inherits as its placeholder (Field.tsx).

import { SwitchField, TextField } from '../Field';

export default function GeneralSection() {
  return (
    <div class="card settings-card">
      <TextField name="name" required />
      <TextField name="git_author_name" spellcheck />
      <TextField name="git_author_email" />
      <SwitchField
        name="incogni"
        description="Strips AI attribution from this repo's output."
        hint="Branch naming stays as it is — adjust the pattern and the prefix in Branches yourself if needed. It cannot hide the forge account of the token used, nor style or timing signals."
      />
    </div>
  );
}
