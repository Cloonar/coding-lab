/* @refresh reload */
import { render } from 'solid-js/web';
import { Navigate, Route, Router } from '@solidjs/router';
import App from './App';
import Credentials from './routes/Credentials';
import History from './routes/History';
import Login from './routes/Login';
import NewRun from './routes/NewRun';
import RepoRoutes from './routes/repo-home/routes';
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
      <Route path="/credentials" component={Credentials} />
      <Route path="/history" component={History} />
      {/* Static /runs redirect must precede the /runs/:id chat route. */}
      <Route path="/runs" component={() => <Navigate href="/history" />} />
      <Route path="/runs/:id" component={RunChat} />
      {/* Optional :section (issue #198): the bare path renders the category
          index on mobile and redirects to the first category on desktop. */}
      <Route path="/settings/:section?" component={Settings} />
      <Route path="/tokens" component={Tokens} />
      {/* /repos, /repos/new and the /repos/:id repo home with its nested tabs
          (issue #61) — one shared table, see routes/repo-home/routes.tsx. */}
      <RepoRoutes />
      <Route path="*" component={() => <Navigate href="/" />} />
    </Router>
  ),
  root,
);
