// littleFedi service worker.
//
// Scope is deliberately narrow: this is an installability + offline-shell
// layer, not an offline app. Pages render server-side per session (CSRF
// tokens, viewer-specific content, moderation state) so HTML is never
// cached - only immutable static assets and the offline fallback are.
//
// app.css, app.js, streaming.js and emoji_map.js fingerprints are injected by
// ServiceWorkerHandler. Bump the prefix when changing other precached assets.
const CACHE_VERSION = 'v10-__STATIC_VERSION__';
const CACHE_NAME = 'littlefedi-' + CACHE_VERSION;

const OFFLINE_URL = '/static/offline.html';

const PRECACHE_URLS = [
  '/static/htmx.min.js',
  '/static/alpine.min.js',
  '__EMOJI_MAP_URL__',
  '__STREAMING_JS_URL__',
  '__APP_CSS_URL__',
  '__APP_JS_URL__',
  // Every theme stylesheet is precached for everyone: they are small, and a
  // themed account would otherwise get an unstyled offline shell. The single
  // placeholder expands to one quoted URL per theme, so this file stays valid
  // JavaScript when read raw. Past ~15KB gzipped in total, stop precaching
  // them all and cache only the active theme on first fetch instead.
  '__THEME_CSS_URLS__',
  '/static/icon-192.png',
  '/static/icon-512.png',
  '/static/icon-512-maskable.png',
  '/static/apple-touch-icon.png',
  '/static/badge-96.png',
  '/static/badge-block-96.png',
  '/static/badge-boost-96.png',
  '/static/badge-favourite-96.png',
  '/static/badge-follow-96.png',
  '/static/badge-move-96.png',
  '/static/badge-poll-96.png',
  '/static/badge-quote-96.png',
  '/static/badge-reply-96.png',
  '/static/favicon.svg',
  '/static/favicon.ico',
  OFFLINE_URL,
];

// addAll is all or nothing across thirty-odd requests: one flaky response and
// install rejects, the worker never activates, and the offline shell quietly
// stops existing (registration errors are swallowed on the page side). Take the
// files one at a time and let the misses go. Only the shell itself has to land.
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then((cache) => Promise.all(PRECACHE_URLS.map((url) => cache.add(url).catch((err) => {
        if (url === OFFLINE_URL) throw err;
      }))))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((names) => Promise.all(
        names.filter((name) => name.startsWith('littlefedi-') && name !== CACHE_NAME)
          .map((name) => caches.delete(name))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  if (request.method !== 'GET') return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  // The manifest has its own no-cache policy and controls installed-app chrome.
  if (request.destination === 'manifest') return;

  // Full page navigations: always go to the network so sessions, CSRF
  // tokens, and moderation-gated content stay live. Only fall back to the
  // cached offline shell when the network is actually unreachable.
  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).catch(() => caches.match(OFFLINE_URL)
        // respondWith throws on undefined, which would replace the browser's
        // own offline page with a broken one if the shell is missing.
        .then((cached) => cached || Response.error()))
    );
    return;
  }

  // Static assets: cache-first, revalidate in the background so a
  // deployed change still reaches clients on the next load.
  if (url.pathname.startsWith('/static/')) {
    event.respondWith(
      caches.match(request).then((cached) => {
        const network = fetch(request).then((response) => {
          if (response.ok) {
            const copy = response.clone();
            caches.open(CACHE_NAME).then((cache) => cache.put(request, copy));
          }
          return response;
        });
        if (cached) {
          event.waitUntil(network.catch(() => undefined));
          return cached;
        }
        return network;
      })
    );
    return;
  }

  // Everything else (API, partials, media, emoji): never intercepted,
  // viewer-specific and moderation-sensitive data must always be fresh.
});

// Web Push delivery: internal/push sends an aes128gcm-encrypted
// NotificationPayload (see internal/push/sender.go) which the browser
// decrypts before this handler ever sees it.
const NOTIFICATION_STYLES = {
  mention:        { badge: 'reply',     symbol: '↩' },
  reblog:         { badge: 'boost',     symbol: '↻' },
  favourite:      { badge: 'favourite', symbol: '♥' },
  quote:          { badge: 'quote',     symbol: '❝' },
  follow:         { badge: 'follow',    symbol: '+' },
  follow_request: { badge: 'follow',    symbol: '+' },
  follow_accept:  { badge: 'follow',    symbol: '✓' },
  follow_reject:  { badge: 'follow',    symbol: '−' },
  unfollow:       { badge: 'follow',    symbol: '−' },
  poll:           { badge: 'poll',      symbol: '▥' },
  move:           { badge: 'move',      symbol: '→' },
  block:          { badge: 'block',     symbol: '⊘' },
  unblock:        { badge: 'block',     symbol: '✓' },
  status:         { badge: '',          symbol: '✎' },
  default:        { badge: '',          symbol: '✎' },
};

self.addEventListener('push', (event) => {
  let payload = {};
  if (event.data) {
    try {
      payload = event.data.json();
    } catch (e) {
      payload = {};
    }
  }
  const style = NOTIFICATION_STYLES[payload.type] || NOTIFICATION_STYLES.default;
  const options = {
    body: payload.body || '',
    icon: payload.icon || '/static/icon-192.png',
    badge: payload.badge || `/static/badge${style.badge ? '-' + style.badge : ''}-96.png`,
    tag: payload.tag || undefined,
    data: { url: payload.url || '/' },
  };
  event.waitUntil(
    self.registration.showNotification(`${style.symbol} ${payload.title || 'littleFedi'}`, options)
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const requested = (event.notification.data && event.notification.data.url) || '/';
  let target = new URL('/', self.location.origin);
  try {
    const parsed = new URL(requested, self.location.origin);
    if (parsed.origin === self.location.origin) target = parsed;
  } catch (e) {
    // Keep the safe root fallback.
  }
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
      for (const client of list) {
        if (client.url === target.href && 'focus' in client) return client.focus();
      }
      if (self.clients.openWindow) return self.clients.openWindow(target.href);
    })
  );
});
