// The service worker: what makes the board installable, and what receives a
// push while the page is closed. It caches nothing — the board is live state
// and a stale copy of it is worse than none.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", e => e.waitUntil(self.clients.claim()));
// A fetch handler, even an empty one, is what older Android Chrome checks for
// before offering "Add to home screen".
self.addEventListener("fetch", () => {});

// One notification per push. The payload is the server's {title, body, url,
// tag}; a tag makes a second push about the same item replace the first
// instead of stacking.
self.addEventListener("push", e => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch { d = { body: e.data ? e.data.text() : "" }; }
  e.waitUntil(self.registration.showNotification(d.title || "Conveyor", {
    body: d.body || "",
    icon: "/icon-192.png",
    badge: "/icon-192.png",
    tag: d.tag || undefined,
    renotify: !!d.tag,
    data: { url: d.url || "/" },
  }));
});

// Tapping a notification opens the item it is about, in the app if it is
// already open.
self.addEventListener("notificationclick", e => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || "/", self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(list => {
    for (const c of list) {
      if ("focus" in c) { c.navigate(url); return c.focus(); }
    }
    return self.clients.openWindow(url);
  }));
});

// The push service rotated the subscription: take a new one and tell the
// server, or the next notification goes nowhere.
self.addEventListener("pushsubscriptionchange", e => {
  e.waitUntil((async () => {
    const { key } = await (await fetch("/api/push/key")).json();
    const sub = await self.registration.pushManager.subscribe({
      userVisibleOnly: true, applicationServerKey: key,
    });
    await fetch("/api/push/subscribe", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(sub),
    });
  })());
});
