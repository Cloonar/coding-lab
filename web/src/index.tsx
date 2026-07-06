/* @refresh reload */
import { render } from 'solid-js/web';
import { Navigate, Route, Router } from '@solidjs/router';
import App from './App';
import AddRepo from './routes/AddRepo';
import CRDetail from './routes/CRDetail';
import Credentials from './routes/Credentials';
import Dashboard from './routes/Dashboard';
import IssueDetail from './routes/IssueDetail';
import Login from './routes/Login';
import NewIssue from './routes/NewIssue';
import RepoCRs from './routes/RepoCRs';
import RepoIssues from './routes/RepoIssues';
import RepoLabels from './routes/RepoLabels';
import RepoSettings from './routes/RepoSettings';
import Runs from './routes/Runs';
import Settings from './routes/Settings';
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
      <Route path="/" component={Dashboard} />
      <Route path="/credentials" component={Credentials} />
      <Route path="/runs" component={Runs} />
      <Route path="/settings" component={Settings} />
      <Route path="/tokens" component={Tokens} />
      <Route path="/repos/new" component={AddRepo} />
      <Route path="/repos/:id/settings" component={RepoSettings} />
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
