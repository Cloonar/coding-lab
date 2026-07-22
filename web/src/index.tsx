/* @refresh reload */
import { render } from 'solid-js/web';
import { Navigate, Route, Router } from '@solidjs/router';
import App from './App';
import AddRepo from './routes/AddRepo';
import CRDetail from './routes/CRDetail';
import Credentials from './routes/Credentials';
import History from './routes/History';
import IssueDetail from './routes/IssueDetail';
import Login from './routes/Login';
import NewIssue from './routes/NewIssue';
import NewRun from './routes/NewRun';
import RepoCRs from './routes/RepoCRs';
import RepoIssues from './routes/RepoIssues';
import RepoLabels from './routes/RepoLabels';
import Repos from './routes/Repos';
import RepoSettings from './routes/repo-settings';
import RunChat from './routes/RunChat';
import Settings from './routes/settings';
import Setup from './routes/Setup';
import Tokens from './routes/Tokens';
import { registerServiceWorker } from './pwa';
import './base.css';

const root = document.getElementById('root');
if (!root) throw new Error('missing #root element');

registerServiceWorker();

render(
  () => (
    <Router root={App}>
      <Route path="/setup" component={Setup} />
      <Route path="/login" component={Login} />
      <Route path="/" component={NewRun} />
      <Route path="/repos" component={Repos} />
      <Route path="/credentials" component={Credentials} />
      <Route path="/history" component={History} />
      {/* Static /runs redirect must precede the /runs/:id chat route. */}
      <Route path="/runs" component={() => <Navigate href="/history" />} />
      <Route path="/runs/:id" component={RunChat} />
      {/* Optional :section (issue #198): the bare path renders the category
          index on mobile and redirects to the first category on desktop. */}
      <Route path="/settings/:section?" component={Settings} />
      <Route path="/tokens" component={Tokens} />
      <Route path="/repos/new" component={AddRepo} />
      <Route path="/repos/:id/settings/:section?" component={RepoSettings} />
      <Route path="/repos/:id/issues" component={RepoIssues} />
      <Route path="/repos/:id/issues/new" component={NewIssue} />
      <Route path="/repos/:id/issues/:number" component={IssueDetail} />
      <Route path="/repos/:id/crs" component={RepoCRs} />
      <Route path="/repos/:id/crs/:number" component={CRDetail} />
      <Route path="/repos/:id/labels" component={RepoLabels} />
      <Route path="*" component={() => <Navigate href="/" />} />
    </Router>
  ),
  root,
);
