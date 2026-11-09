/* Streamlink Studio service worker.
 * Network-first for the app shell (so updates show up immediately), with the
 * last good copy as an offline fallback. Data (/api, /metrics, video streams,
 * downloads) is NEVER cached: it is private and changes constantly. */
const CACHE = 'studio-shell-v1';
const scopePath = new URL(self.registration.scope).pathname;
const NEVER = [/^\/api\//, /^\/metrics/, /^\/healthz/];

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil((async () => {
  for (const k of await caches.keys()) if (k !== CACHE) await caches.delete(k);
  await self.clients.claim();
})()));

self.addEventListener('fetch', (e) => {
  const r = e.request;
  if (r.method !== 'GET' || r.headers.has('range')) return;
  const u = new URL(r.url);
  if (u.origin !== location.origin) return;
  const rel = u.pathname.startsWith(scopePath) ? '/' + u.pathname.slice(scopePath.length) : u.pathname;
  if (NEVER.some((re) => re.test(rel))) return;
  e.respondWith((async () => {
    try {
      const res = await fetch(r);
      // only plain 200s: never keep redirects (e.g. to the login page) or errors
      if (res.ok && !res.redirected && res.type === 'basic') {
        const c = await caches.open(CACHE);
        c.put(r, res.clone()).catch(() => {});
      }
      return res;
    } catch {
      const hit = await caches.match(r, { ignoreSearch: r.mode === 'navigate' });
      if (hit) return hit;
      if (r.mode === 'navigate') {
        const shell = (await caches.match(scopePath)) || (await caches.match(scopePath + 'index.html'));
        if (shell) return shell;
      }
      return new Response('Offline', { status: 503, headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
    }
  })());
});
