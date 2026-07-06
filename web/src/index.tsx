/* @refresh reload */
import { render } from 'solid-js/web';
import { Navigate, Route, Router } from '@solidjs/router';
import App from './App';
import Dashboard from './routes/Dashboard';
import Login from './routes/Login';
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
      <Route path="*" component={() => <Navigate href="/" />} />
    </Router>
  ),
  root,
);
