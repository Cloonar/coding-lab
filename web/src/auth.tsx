// Auth-state context: one authState() resource shared by every route guard.

import {
  createContext,
  createResource,
  useContext,
  type ParentProps,
  type Resource,
} from 'solid-js';
import { authState, type AuthState } from './api';

export interface AuthContextValue {
  auth: Resource<AuthState>;
  /** Re-fetches /auth/state and resolves with the fresh value. */
  refresh: () => Promise<AuthState | undefined>;
}

const AuthContext = createContext<AuthContextValue>();

export function AuthProvider(props: ParentProps) {
  const [auth, { refetch }] = createResource(() => authState());
  const refresh = async (): Promise<AuthState | undefined> => {
    const value = await refetch();
    return value ?? undefined;
  };
  return <AuthContext.Provider value={{ auth, refresh }}>{props.children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used inside <AuthProvider>');
  return ctx;
}
