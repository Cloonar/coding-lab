/* @refresh reload */
// SPA entry: registers the service worker, records the boot history entry and
// mounts the router with the app's route map (issue #76 for its current shape).
import { render } from 'solid-js/web';
import { Navigate, Route, Router } from '@solidjs/router';
import App from './App';
import Credentials from './routes/Credentials';
import History from './routes/History';
import Login from './routes/Login';
import More from './routes/More';
import NewRun from './routes/NewRun';
import RepoRoutes from './routes/repo-home/routes';
import RunChat from './routes/RunChat';
import Runs from './routes/Runs';
import Settings from './routes/settings';
import Setup from './routes/Setup';
import Tokens from './routes/Tokens';
import { initNavHistory } from './lib/navHistory';
import { registerServiceWorker } from './pwa';
import './base.css';

const root = document.getElementById('root');
if (!root) throw new Error('missing #root element');

registerServiceWorker();
// Record the app's first history entry before any in-app navigation, so the
// chat's Back knows whether it has an in-app entry to pop (issue #76).
initNavHistory();

render(
  () => (
    <Router root={App}>
      <Route path="/setup" component={Setup} />
      <Route path="/login" component={Login} />
      {/* One route map for every width (issue #76): Runs is home at `/`, the
          composer moved to `/new`, More is the phone's fourth tab (it
          redirects to /settings on desktop), and History stays at /history
          as the Runs page's Ended view. */}
      <Route path="/" component={Runs} />
      <Route path="/new" component={NewRun} />
      <Route path="/more" component={More} />
      <Route path="/credentials" component={Credentials} />
      <Route path="/history" component={History} />
      {/* Static /runs redirect (to Runs since issue #76, was /history) must
          precede the /runs/:id chat route. */}
      <Route path="/runs" component={() => <Navigate href="/" />} />
      <Route path="/runs/:id" component={RunChat} />
      {/* Optional :section (issue #198): the bare path renders the category
          index on mobile and redirects to the first category on desktop. */}
      <Route path="/settings/:section?" component={Settings} />
      <Route path="/tokens" component={Tokens} />
      {/* /repos, /repos/new and the /repos/:id repo home with its nested tabs
          (issue #61) — one shared table, see routes/repo-home/routes.tsx. */}
      <RepoRoutes />
      {/* Anything unknown lands on Runs, the home page. */}
      <Route path="*" component={() => <Navigate href="/" />} />
    </Router>
  ),
  root,
);
