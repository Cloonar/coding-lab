// Typed fetch client for the lab operator API (/api/v1).
//
// This module is a barrel: the client is split into domain modules under
// web/src/api/, one per section of the original file, and re-exported here so
// every `'../api'` importer stays untouched. The original per-request, CSRF,
// and error-envelope documentation now lives in api/core.ts alongside the
// shared `request` helper.

export * from './api/core';
export * from './api/auth';
export * from './api/credentials';
export * from './api/repos';
export * from './api/imports';
export * from './api/providers';
export * from './api/runs';
export * from './api/chat';
export * from './api/spawn';
export * from './api/issues';
export * from './api/secrets';
export * from './api/schedules';
export * from './api/afk';
export * from './api/autoland';
export * from './api/tokens';
export * from './api/push';
export * from './api/settings';
export * from './api/crs';
