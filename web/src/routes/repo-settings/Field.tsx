// The repo settings field vocabulary (issue #61): the shared settings field
// components (components/settings/Field.tsx — what a field shows, how it is
// marked, where its problem goes), typed for the repo field table
// (fields.ts), so a section's `<TextField name="git_author_name" …/>` names a
// real repo field whose draft is text. The sections import from here.

import { fieldComponents } from '../../components/settings/Field';
import type { RepoSettingsShape } from './fields';

export { FieldGroup, fieldControlId, inheritPickLabel } from '../../components/settings/Field';

export const { Field, TextField, SelectField, NativeSelectField, SegmentedField, SwitchField } =
  fieldComponents<RepoSettingsShape>();
