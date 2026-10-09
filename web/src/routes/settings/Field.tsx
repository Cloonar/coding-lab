// The global settings field vocabulary (issue #85): the shared settings field
// components (components/settings/Field.tsx — what a field shows, how it is
// marked, where its problem goes), typed for the global field table
// (fields.ts), so a section's `<TextField name="git_author_name" …/>` names a
// real settings key whose draft is text. The sections import from here.

import { fieldComponents } from '../../components/settings/Field';
import type { GlobalSettingsShape } from './fields';

export { FieldGroup, fieldControlId, inheritPickLabel } from '../../components/settings/Field';

export const { Field, TextField, SelectField, SegmentedField, SwitchField } =
  fieldComponents<GlobalSettingsShape>();
