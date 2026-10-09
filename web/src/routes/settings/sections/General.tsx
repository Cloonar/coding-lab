// Global settings › General (issues #198, #81, #85, #90): the git author
// identity commits use unless a repo overrides it, how long ended runs keep
// their transcript, and whether a merge deletes the PR/CR head branch on
// origin (global only, no per-repo override; ADR-0081) — a thin renderer over
// the form store. Every control edits a draft; the page's save bar sends the
// changed ones (form.tsx, fields.ts).
// Transcript retention is a whole number from 0 (keep none) to the server's
// cap, checked at the field before anything is sent.
//
// The section ends with two read-only status cards, outside the saved fields:
// the credential gateway (issue #23, the OneCLI sidecar) and the SSH bastion
// (issue #39, the Warpgate sidecar). They hold nothing Save could commit; they
// say whether lab can reach what its runs depend on.

import { TRANSCRIPT_RETENTION_MAX_DAYS } from '../../../api';
import BastionStatus from '../../../components/BastionStatus';
import CredentialGatewayStatus from '../../../components/CredentialGatewayStatus';
import { FieldGroup, SwitchField, TextField } from '../Field';

export default function GeneralSection() {
  return (
    <>
      <div class="card settings-card">
        <FieldGroup title="Git author">
          <TextField
            name="git_author_name"
            spellcheck
            hint="Used for commits unless a repo overrides it."
          />
          <TextField name="git_author_email" />
        </FieldGroup>
        <FieldGroup title="Transcripts">
          <TextField
            name="transcript_retention_days"
            type="number"
            hint={`Ended runs keep their transcript for this many days; 0 keeps none (max ${TRANSCRIPT_RETENTION_MAX_DAYS}).`}
          />
        </FieldGroup>
        <FieldGroup title="Merging">
          <SwitchField
            name="merge_delete_head"
            hint="Deletes the PR/CR head branch on origin after a merge; local branches are unaffected."
          />
        </FieldGroup>
      </div>
      <CredentialGatewayStatus />
      <BastionStatus />
    </>
  );
}
