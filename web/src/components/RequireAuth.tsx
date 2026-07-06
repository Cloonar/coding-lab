// Route guard shared by every authenticated page: bounce to /setup while
// first-run setup is pending, to /login without a session, else render.

import { Navigate } from '@solidjs/router';
import { Match, Switch, type ParentProps } from 'solid-js';
import { useAuth } from '../auth';

export default function RequireAuth(props: ParentProps) {
  const { auth } = useAuth();
  return (
    <Switch>
      <Match when={auth()?.setup_required}>
        <Navigate href="/setup" />
      </Match>
      <Match when={auth() && !auth()!.authenticated}>
        <Navigate href="/login" />
      </Match>
      <Match when={auth()?.authenticated}>{props.children}</Match>
    </Switch>
  );
}
