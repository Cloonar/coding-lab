// resourceValue contract: a ready resource reads through; an errored one
// answers undefined instead of re-throwing (a raw read throws, which is
// exactly what blanks a route when it happens outside the guarded <Match>).

import { createResource, createRoot } from 'solid-js';
import { describe, expect, it } from 'vitest';
import { resourceValue } from './resource';

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

describe('resourceValue', () => {
  it('returns the value once the resource resolves', async () => {
    const [res, dispose] = createRoot((d) => {
      const [r] = createResource(() => Promise.resolve('ok'));
      return [r, d] as const;
    });

    expect(resourceValue(res)).toBeUndefined(); // still loading
    await flush();
    expect(resourceValue(res)).toBe('ok');
    dispose();
  });

  it('returns undefined for an errored resource where a raw read throws', async () => {
    const [res, dispose] = createRoot((d) => {
      const [r] = createResource<string>(() => Promise.reject(new Error('boom')));
      return [r, d] as const;
    });
    await flush();

    expect(() => res()).toThrow('boom');
    expect(resourceValue(res)).toBeUndefined();
    dispose();
  });
});
