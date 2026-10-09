// The repo page's leave guard (issue #61): the shared settings leave dialog
// (components/settings/LeaveGuard.tsx) with the repo home as its area —
// anything under /repos/:id/… is inside (tab switches, section URLs, issue
// pages, the schedule editor URLs), so only leaving the repo with pending
// settings changes asks. The dialog names the repo the changes are to.

import LeaveGuard, { isInsidePath } from '../../components/settings/LeaveGuard';
import { useRepoHome } from '../repo-home/context';
import { useRepoSettingsForm } from './form';

/** Whether a URL (path, with or without query and hash) is inside one repo's home. */
export function isInsideRepo(url: string, repoID: string): boolean {
  return isInsidePath(url, `/repos/${repoID}`);
}

export default function RepoLeaveGuard() {
  const home = useRepoHome();
  const form = useRepoSettingsForm();
  return (
    <LeaveGuard
      inside={(url) => isInsideRepo(url, home.id())}
      subject={form.saved()?.name ?? 'this repository'}
    />
  );
}
