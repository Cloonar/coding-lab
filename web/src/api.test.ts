import { createResource, createRoot } from 'solid-js';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ApiError,
  authState,
  createCredential,
  createRepo,
  deleteCredential,
  deleteRepo,
  errorMessage,
  listCredentials,
  listRepos,
  login,
  logout,
  me,
  retryClone,
  setUnauthorizedHandler,
  updateCredential,
  updateRepo,
} from './api';

interface FakeResponse {
  ok: boolean;
  status: number;
  json(): Promise<unknown>;
  text(): Promise<string>;
}

function jsonResponse(status: number, body?: unknown): FakeResponse {
  const text = body === undefined ? '' : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => {
      if (text === '') return Promise.reject(new SyntaxError('empty body'));
      return Promise.resolve(JSON.parse(text));
    },
    text: () => Promise.resolve(text),
  };
}

function stubFetch(response: FakeResponse) {
  const mock = vi.fn(() => Promise.resolve(response));
  vi.stubGlobal('fetch', mock);
  return mock;
}

function fetchCall(mock: ReturnType<typeof stubFetch>): [string, RequestInit] {
  return mock.mock.calls[0] as unknown as [string, RequestInit];
}

function requestInit(mock: ReturnType<typeof stubFetch>): RequestInit {
  return fetchCall(mock)[1];
}

afterEach(() => {
  vi.unstubAllGlobals();
  setUnauthorizedHandler(null);
});

describe('api client', () => {
  it('GET authState hits /api/v1/auth/state without a CSRF header and parses the payload', async () => {
    const mock = stubFetch(
      jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
    );

    const state = await authState();

    expect(state).toEqual({ setup_required: false, authenticated: true, username: 'dominik' });
    expect(mock).toHaveBeenCalledTimes(1);
    expect(fetchCall(mock)[0]).toBe('/api/v1/auth/state');
    const init = requestInit(mock);
    expect(init.method).toBe('GET');
    expect(init.credentials).toBe('same-origin');
    expect(init.headers).not.toHaveProperty('X-Lab-Csrf');
  });

  it('POST login sends X-Lab-Csrf: 1, same-origin credentials and a JSON body', async () => {
    const mock = stubFetch(jsonResponse(204));

    await login('dominik', 'hunter22', true);

    const init = requestInit(mock);
    expect(fetchCall(mock)[0]).toBe('/api/v1/auth/login');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('same-origin');
    expect(init.headers).toMatchObject({
      'X-Lab-Csrf': '1',
      'Content-Type': 'application/json',
    });
    expect(JSON.parse(init.body as string)).toEqual({
      username: 'dominik',
      password: 'hunter22',
      remember: true,
    });
  });

  it('POST logout carries the CSRF header even without a body', async () => {
    const mock = stubFetch(jsonResponse(204));

    await logout();

    const init = requestInit(mock);
    expect(init.headers).toMatchObject({ 'X-Lab-Csrf': '1' });
    expect(init.headers).not.toHaveProperty('Content-Type');
    expect(init.body).toBeUndefined();
  });

  it('turns the {"error"} envelope into ApiError(status, message)', async () => {
    stubFetch(jsonResponse(401, { error: 'invalid credentials' }));

    const err = await login('dominik', 'wrong', false).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect((err as ApiError).message).toBe('invalid credentials');
  });

  it('falls back to a generic message when the error body is not JSON', async () => {
    stubFetch({
      ok: false,
      status: 502,
      json: () => Promise.reject(new SyntaxError('not json')),
      text: () => Promise.resolve('bad gateway'),
    });

    const err = await authState().catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(502);
    expect((err as ApiError).message).toBe('Request failed (502)');
  });

  it('invokes the unauthorized handler on 401 and not on other failures', async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);

    stubFetch(jsonResponse(401, { error: 'session expired' }));
    await me().catch(() => undefined);
    expect(handler).toHaveBeenCalledTimes(1);

    stubFetch(jsonResponse(400, { error: 'bad request' }));
    await me().catch(() => undefined);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('never invokes the unauthorized handler for /auth/state itself', async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    stubFetch(jsonResponse(401, { error: 'unauthorized' }));

    const err = await authState().catch((e: unknown) => e);

    // The caller sees the 401 and treats it as unauthenticated; the handler
    // (which refetches /auth/state) must not fire, or a gateway that 401s
    // everything would loop the app forever.
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect(handler).not.toHaveBeenCalled();
  });

  it('settles into a stable errored auth state when every request 401s — no refetch loop', async () => {
    // Regression: an auth gateway with an expired session answers 401 to
    // every request, /auth/state included. The App wiring (401 handler →
    // refetch of the auth resource) must converge after the initial fetch
    // instead of refetching /auth/state once per round-trip forever.
    //
    // The stub resolves a macrotask later: solid's `scheduled` flag only
    // suppresses refetches within one microtask turn, so 401s that arrive a
    // genuine round-trip apart are what makes the buggy version loop.
    const mock = vi.fn(
      () =>
        new Promise<FakeResponse>((resolve) => {
          setTimeout(() => resolve(jsonResponse(401, { error: 'unauthorized' })), 0);
        }),
    );
    vi.stubGlobal('fetch', mock);

    await createRoot(async (dispose) => {
      const [auth, { refetch }] = createResource(() => authState());
      // Mirror App.tsx's global handler: any API 401 refreshes the resource.
      setUnauthorizedHandler(() => {
        void Promise.resolve(refetch()).catch(() => undefined);
      });

      // Each turn spans a macrotask, i.e. one full 401 round-trip: a refetch
      // loop would add one fetch call per turn.
      for (let i = 0; i < 10; i += 1) {
        await new Promise((resolve) => setTimeout(resolve, 0));
      }

      /* eslint-disable solid/reactivity -- imperative assertions on the settled resource */
      expect(auth.state).toBe('errored');
      expect(auth.error).toBeInstanceOf(ApiError);
      expect((auth.error as ApiError).status).toBe(401);
      /* eslint-enable solid/reactivity */
      expect(mock).toHaveBeenCalledTimes(1); // bounded: the initial fetch only

      dispose();
    });
  });
});

describe('credential endpoints', () => {
  it('POST /credentials sends {name, kind, payload} with the CSRF header', async () => {
    const created = {
      id: 'cred_1',
      name: 'deploy-key',
      kind: 'ssh_key',
      created_at: '2026-07-06T00:00:00.000Z',
      updated_at: '2026-07-06T00:00:00.000Z',
    };
    const mock = stubFetch(jsonResponse(201, created));

    const result = await createCredential('deploy-key', 'ssh_key', {
      private_key: 'PEM',
      passphrase: 'pw',
    });

    expect(result).toEqual(created);
    expect(fetchCall(mock)[0]).toBe('/api/v1/credentials');
    const init = requestInit(mock);
    expect(init.method).toBe('POST');
    expect(init.headers).toMatchObject({ 'X-Lab-Csrf': '1' });
    expect(JSON.parse(init.body as string)).toEqual({
      name: 'deploy-key',
      kind: 'ssh_key',
      payload: { private_key: 'PEM', passphrase: 'pw' },
    });
  });

  it('GET /credentials unwraps the {credentials} envelope', async () => {
    const item = {
      id: 'cred_1',
      name: 'deploy-key',
      kind: 'ssh_key',
      created_at: '2026-07-06T00:00:00.000Z',
      updated_at: '2026-07-06T00:00:00.000Z',
      referenced: true,
    };
    const mock = stubFetch(jsonResponse(200, { credentials: [item] }));

    const result = await listCredentials();

    expect(result).toEqual([item]);
    expect(fetchCall(mock)[0]).toBe('/api/v1/credentials');
    expect(requestInit(mock).method).toBe('GET');
  });

  it('PATCH /credentials/{id} rotates the payload without ever reading it back', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        id: 'cred_1',
        name: 'deploy-key',
        kind: 'https_token',
        created_at: '2026-07-06T00:00:00.000Z',
        updated_at: '2026-07-06T01:00:00.000Z',
      }),
    );

    await updateCredential('cred_1', { payload: { username: 'dominik', token: 't0k' } });

    expect(fetchCall(mock)[0]).toBe('/api/v1/credentials/cred_1');
    const init = requestInit(mock);
    expect(init.method).toBe('PATCH');
    expect(JSON.parse(init.body as string)).toEqual({
      payload: { username: 'dominik', token: 't0k' },
    });
  });

  it('DELETE /credentials/{id} surfaces the referenced-409 message verbatim', async () => {
    stubFetch(jsonResponse(409, { error: 'credential is referenced by 2 repositories' }));

    const err = await deleteCredential('cred_1').catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).message).toBe('credential is referenced by 2 repositories');
  });
});

describe('repo endpoints', () => {
  it('POST /repos sends the create request as-is (omitted fields stay omitted)', async () => {
    const mock = stubFetch(jsonResponse(201, { id: 'repo_1', clone_status: 'cloning' }));

    await createRepo({ remote_url: 'git@h:o/r.git', credential_id: 'cred_1', incogni: true });

    expect(fetchCall(mock)[0]).toBe('/api/v1/repos');
    const init = requestInit(mock);
    expect(init.method).toBe('POST');
    expect(init.headers).toMatchObject({ 'X-Lab-Csrf': '1' });
    expect(JSON.parse(init.body as string)).toEqual({
      remote_url: 'git@h:o/r.git',
      credential_id: 'cred_1',
      incogni: true,
    });
  });

  it('GET /repos unwraps the {repos} envelope', async () => {
    const mock = stubFetch(jsonResponse(200, { repos: [{ id: 'repo_1' }] }));

    const result = await listRepos();

    expect(result).toEqual([{ id: 'repo_1' }]);
    expect(fetchCall(mock)[0]).toBe('/api/v1/repos');
  });

  it('PATCH /repos/{id} sends only the given fields, null meaning "clear"', async () => {
    const mock = stubFetch(jsonResponse(200, { id: 'repo_1' }));

    await updateRepo('repo_1', { budget_minutes: null, afk_auto_enabled: true });

    expect(fetchCall(mock)[0]).toBe('/api/v1/repos/repo_1');
    const init = requestInit(mock);
    expect(init.method).toBe('PATCH');
    expect(JSON.parse(init.body as string)).toEqual({
      budget_minutes: null,
      afk_auto_enabled: true,
    });
  });

  it('DELETE /repos/{id} appends ?force=true only when forcing', async () => {
    let mock = stubFetch(jsonResponse(204));
    await deleteRepo('repo_1');
    expect(fetchCall(mock)[0]).toBe('/api/v1/repos/repo_1');

    mock = stubFetch(jsonResponse(204));
    await deleteRepo('repo_1', true);
    expect(fetchCall(mock)[0]).toBe('/api/v1/repos/repo_1?force=true');
    expect(requestInit(mock).method).toBe('DELETE');
  });

  it('POST /repos/{id}/clone/retry tolerates an empty 202 body', async () => {
    const mock = stubFetch({
      ok: true,
      status: 202,
      json: () => Promise.reject(new SyntaxError('empty body')),
      text: () => Promise.resolve(''),
    });

    await expect(retryClone('repo_1')).resolves.toBeUndefined();
    expect(fetchCall(mock)[0]).toBe('/api/v1/repos/repo_1/clone/retry');
    expect(requestInit(mock).method).toBe('POST');
  });
});

describe('errorMessage', () => {
  it('surfaces the real ApiError message', () => {
    expect(errorMessage(new ApiError(409, 'credential in use'))).toBe('credential in use');
  });

  it('maps fetch TypeErrors to the network hint', () => {
    expect(errorMessage(new TypeError('fetch failed'))).toBe(
      'Network error — is lab still running?',
    );
  });

  it('maps anything else to the generic message', () => {
    expect(errorMessage(new Error('boom'))).toBe('Something went wrong — please try again.');
  });
});
