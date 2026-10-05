// The repositories route tree (issue #61), shared by the app's router
// (src/index.tsx) and the route tests, so a test mounts the real table:
//
//   /repos                        Repos — the list
//   /repos/new                    AddRepo
//   /repos/:id                    RepoHome frame, with nested tabs:
//     (index)                     Overview
//     issues | issues/new | issues/:number
//     crs | crs/:number           (the CRs tab shows for builtin-bound repos)
//     labels
//     settings/:section?/:scheduleId?
//       (settings/schedules/new | settings/schedules/:scheduleId open the
//        schedule editor over the settings page — one route, so the page
//        stays mounted while the editor comes and goes)
//
// @solidjs/router ranks static segments above params, so /repos/new beats
// /repos/:id regardless of order. A component returning <Route> elements is
// plain route config: the router flattens it like inline children.

import { Route } from '@solidjs/router';
import AddRepo from '../AddRepo';
import CRDetail from '../CRDetail';
import IssueDetail from '../IssueDetail';
import NewIssue from '../NewIssue';
import RepoCRs from '../RepoCRs';
import RepoIssues from '../RepoIssues';
import RepoLabels from '../RepoLabels';
import Repos from '../Repos';
import RepoSettings from '../repo-settings';
import RepoHome from './index';
import Overview from './Overview';

export default function RepoRoutes() {
  return (
    <>
      <Route path="/repos" component={Repos} />
      <Route path="/repos/new" component={AddRepo} />
      <Route path="/repos/:id" component={RepoHome}>
        <Route path="/" component={Overview} />
        <Route path="/issues" component={RepoIssues} />
        <Route path="/issues/new" component={NewIssue} />
        <Route path="/issues/:number" component={IssueDetail} />
        <Route path="/crs" component={RepoCRs} />
        <Route path="/crs/:number" component={CRDetail} />
        <Route path="/labels" component={RepoLabels} />
        {/* One route for the settings page AND the schedule editor's URLs
            (settings/schedules/new, settings/schedules/:scheduleId): the
            page reads :scheduleId only under the schedules section, and
            stays mounted — scrolled where it was — while the editor opens
            over it and closes again. */}
        <Route path="/settings/:section?/:scheduleId?" component={RepoSettings} />
      </Route>
    </>
  );
}
