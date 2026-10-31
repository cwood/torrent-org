// Present only so browsers offer to install the app. Everything is served
// live by the local service, so nothing is cached.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));
