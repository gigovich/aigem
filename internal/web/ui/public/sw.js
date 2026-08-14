// The service worker exists for one thing: showing a notification when a bot
// starts waiting on the operator and no page is open. It caches nothing and
// intercepts no requests - an offline copy of a conversation that is being
// written to by five bots would be a lie, and the daemon is on a tailnet the
// phone either has or does not.
//
// It is served from the origin root so its scope covers every screen. Vite
// copies public/ to the root of the bundle, and internal/web serves that.

self.addEventListener("install", () => {
  // Take over without waiting for every old tab to close. There is no cached
  // asset to invalidate, so the new worker is never incompatible with a page
  // the old one was serving.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
  event.waitUntil(onPush(event.data));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || "/chat";
  event.waitUntil(openThread(url));
});

// The browser rotates a subscription on its own - a key it decides to replace,
// a service that expires the endpoint - and the daemon then holds one that can
// never be delivered to. Re-subscribing here is the only chance to repair that
// without a page: by definition nobody is looking.
self.addEventListener("pushsubscriptionchange", (event) => {
  event.waitUntil(resubscribe(event));
});

// onPush shows what arrived. A push with no payload, or one this worker cannot
// read, still has to raise something: the browser revokes the subscription of a
// worker that receives a push and shows nothing.
async function onPush(data) {
  let payload;
  try {
    payload = data ? data.json() : {};
  } catch {
    payload = {};
  }
  // Nothing is shown while a window of this application is on screen: that page
  // raises its own notification, and the two would be a duplicate for a
  // conversation the operator is already looking at. A visible client is also
  // what lets a worker stay silent without the browser counting it against the
  // subscription.
  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  if (windows.some((client) => client.visibilityState === "visible")) return;

  return self.registration.showNotification(payload.title || "aigem", {
    body: payload.body || "needs you",
    // Tagged by thread, so a second alert about one conversation replaces the
    // first rather than stacking behind it.
    tag: payload.thread || "aigem",
    data: { url: payload.url || "/chat" },
  });
}

// openThread reuses a window the operator already has rather than opening a
// second copy of the application - and navigates it, because a focused tab
// showing the inbox is not the thread the notification was about.
async function openThread(url) {
  const target = new URL(url, self.location.origin);
  // The payload is authenticated by the subscription's own keys, so a push
  // service cannot forge one - but a notification that can be made to open
  // another origin is not a thing to leave available for the sake of one line.
  if (target.origin !== self.location.origin) return;

  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  for (const client of windows) {
    if (new URL(client.url).origin !== target.origin) continue;
    if (client.url !== target.href && "navigate" in client) {
      try {
        const navigated = await client.navigate(target.href);
        if (navigated) return navigated.focus();
      } catch {
        // navigate() rejects for a client this worker does not control. Focus
        // it anyway: the wrong screen in the right window beats a click that
        // does nothing at all.
      }
    }
    return client.focus();
  }
  return self.clients.openWindow(target.href);
}

// resubscribe replaces a rotated subscription and tells the daemon.
//
// The new subscription is made with the key the old one carried: the daemon's
// key is not reachable from here without a request, and a rotation is not a
// reason to assume it changed. If the browser did not say what the old key was,
// there is nothing to subscribe with and the next page load repairs it.
async function resubscribe(event) {
  const key = event.oldSubscription && event.oldSubscription.options
    ? event.oldSubscription.options.applicationServerKey
    : null;
  if (!key) return;

  const sub =
    event.newSubscription ||
    (await self.registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: key,
    }));
  const json = sub.toJSON();
  const stored = await fetch("/api/chat/push/subs", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    // The cookie the page traded its token for. A worker has no token of its
    // own, and this is the one request it makes.
    credentials: "same-origin",
    body: JSON.stringify({
      endpoint: sub.endpoint,
      p256dh: json.keys ? json.keys.p256dh : "",
      auth: json.keys ? json.keys.auth : "",
    }),
  });
  // A daemon restart revokes every cookie, so this can be refused - and then
  // the daemon knows only the old endpoint. Leaving it is the recoverable
  // failure: it keeps pushing at something dead until the next page load
  // repairs both ends, where dropping it would leave nothing to repair from.
  if (!stored.ok) return;

  if (event.oldSubscription && event.oldSubscription.endpoint !== sub.endpoint) {
    await fetch("/api/chat/push/subs", {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify({ endpoint: event.oldSubscription.endpoint }),
    });
  }
}
