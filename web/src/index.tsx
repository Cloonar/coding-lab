/* @refresh reload */
import { render } from 'solid-js/web';
import { Navigate, Route, Router } from '@solidjs/router';
import App from './App';
import AddRepo from './routes/AddRepo';
import Credentials from './routes/Credentials';
import Dashboard from './routes/Dashboard';
import Login from './routes/Login';
import RepoSettings from './routes/RepoSettings';
import Runs from './routes/Runs';
import Setup from './routes/Setup';
import './base.css';

const root = document.getElementById('root');
if (!root) throw new Error('missing #root element');

render(
  () => (
    <Router root={App}>
      <Route path="/setup" component={Setup} />
      <Route path="/login" component={Login} />
      <Route path="/" component={Dashboard} />
      <Route path="/credentials" component={Credentials} />
      <Route path="/runs" component={Runs} />
      <Route path="/repos/new" component={AddRepo} />
      <Route path="/repos/:id/settings" component={RepoSettings} />
      <Route path="*" component={() => <Navigate href="/" />} />
    </Router>
  ),
  root,
);
