// Service-worker registration (PWA groundwork, D5). Production builds only:
// the Vite dev server does not serve /sw.js from public/ through the SPA
// entry, and a dev worker would shadow HMR traffic with stale shells.

export function registerServiceWorker(): void {
  if (!import.meta.env.PROD) return;
  if (!('serviceWorker' in navigator)) return;
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch((err: unknown) => {
      console.warn('service worker registration failed', err);
    });
  });
}
