// Browser-app support: service worker registration and the install prompt.
let deferred = null;
const listeners = new Set();
const notify = () => listeners.forEach((f) => { try { f(); } catch { /* ignore */ } });

window.addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); deferred = e; notify(); });
window.addEventListener('appinstalled', () => { deferred = null; notify(); });

export function registerSW() {
  // service workers (and so installability) need HTTPS or localhost
  if ('serviceWorker' in navigator && window.isSecureContext) {
    navigator.serviceWorker.register('sw.js').catch(() => { /* not fatal */ });
  }
}

export function installState() {
  return {
    standalone: matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,
    canInstall: !!deferred,
    secure: window.isSecureContext,
    ios: /iphone|ipad|ipod/i.test(navigator.userAgent),
  };
}

export async function promptInstall() {
  if (!deferred) return false;
  const p = deferred;
  deferred = null;
  p.prompt();
  const r = await p.userChoice;
  notify();
  return r.outcome === 'accepted';
}

export function onInstallChange(f) { listeners.add(f); return () => listeners.delete(f); }

// forget the cached app shell (called on sign-out)
export async function clearAppCache() {
  try { for (const k of await caches.keys()) await caches.delete(k); } catch { /* ignore */ }
}
