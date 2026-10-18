// sw.js: the service worker of the installed app, served at /-/sw.js with
// Service-Worker-Allowed: / (see serviceworker.go).
//
// Browsers want an installed app to have a service worker with a fetch handler
// (Chrome on Android mints a WebAPK for such sites, and drops an empty handler
// as if there were none). A forge has nothing to cache: pages are rendered per
// caller (the owner sees private repositories), diffs and logs are live. So
// this worker stores nothing. A page navigation goes to the network as it would
// without the worker; only when the network is unreachable is the visitor shown
// a short offline page instead of the browser's error. Every other request
// (assets, the event stream, Git, POSTs) is left to the browser: the handler
// does not answer it.
//
// The browser fetches this file again on each navigation (at least once a day)
// and installs the new version when a single byte differs; edit it to ship a
// new version. The worker takes over at once (skipWaiting, claim): it holds no
// state.
'use strict';

const OFFLINE = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>ermite is unreachable</title></head>
<body style="font: 16px/1.5 system-ui, sans-serif; max-width: 32rem; margin: 20vh auto; padding: 0 1rem">
<h1 style="font-size: 1.25rem">ermite is unreachable</h1>
<p>The server does not answer. Check the connection, then reload.</p>
</body></html>`;

self.addEventListener('install', () => self.skipWaiting());

self.addEventListener('activate', (event) => {
  // Navigation preload starts the page request while the worker boots, so the
  // worker adds no latency to a navigation.
  event.waitUntil(Promise.all([
    self.registration.navigationPreload?.enable(),
    self.clients.claim(),
  ]));
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.mode !== 'navigate' || req.method !== 'GET') return;
  event.respondWith((async () => {
    try {
      return (await event.preloadResponse) || (await fetch(req));
    } catch {
      return new Response(OFFLINE, {
        status: 503,
        headers: { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' },
      });
    }
  })());
});
