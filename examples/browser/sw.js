// A service worker, so the page itself loads while the server is down:
// files come from the network when it is there, from this cache when not.
const files = ["/", "/app.wasm", "/wasm_exec.js"];

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open("app").then((c) => c.addAll(files)).then(() => self.skipWaiting()));
});
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));
self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET") return;
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        const copy = res.clone();
        caches.open("app").then((c) => c.put(e.request, copy));
        return res;
      })
      .catch(() => caches.match(e.request)),
  );
});
