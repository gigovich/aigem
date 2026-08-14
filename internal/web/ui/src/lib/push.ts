import { useEffect } from "react";
import { api } from "./protocol";
import type { Permission } from "./notify";

/** Web Push: the half of notifications that works with no page open.
 *
 *  Everything here is best-effort. A browser without a service worker, a
 *  daemon without keys and a person who declined all reach the same place - the
 *  tab title still carries the count - so nothing in this file throws its way
 *  into the screen. */

const SW_PATH = "/sw.js";

/** What the daemon answers at /api/chat/push: whether it can be pushed from at
 *  all, and the key to subscribe against. `available` is false on a daemon
 *  whose keys would not load. */
export interface PushAvailability {
  available: boolean;
  key?: string;
}

/** Where the subscription stands.
 *
 *  - `unsupported`: this browser has no service worker or no push manager. On
 *    iOS that is every tab; only an installed home-screen app has them.
 *  - `unavailable`: the daemon has no application server keys.
 *  - `on`: the daemon holds a subscription for this browser.
 *
 *  Nothing draws these. They are what `enable` reports, so a caller - today
 *  only its test - can tell "declined" from "this build cannot". */
export type PushState = "unsupported" | "unavailable" | "on";

export function pushSupported(): boolean {
  return (
    typeof navigator !== "undefined" &&
    "serviceWorker" in navigator &&
    typeof window !== "undefined" &&
    "PushManager" in window
  );
}

/** registration is the worker this page talks to, once. Registering is
 *  idempotent in the browser, but the promise is kept so the in-page
 *  notification path can ask whether there is one without starting another. */
let registration: Promise<ServiceWorkerRegistration> | null = null;

export function register(): Promise<ServiceWorkerRegistration> {
  // A rejected promise is not kept. `??=` only tests for null, so a registration
  // that failed once - a blip fetching /sw.js - would otherwise be handed to
  // every later caller, including the one `existing()` exists to serve: the
  // load that has to unsubscribe a revoked browser and cannot even look one up.
  registration ??= navigator.serviceWorker.register(SW_PATH).catch((e: unknown) => {
    registration = null;
    throw e;
  });
  return registration;
}

/** existing is the worker this browser already has for this origin, whether or
 *  not this page load is the one that registered it.
 *
 *  A page that never called `enable` still has to be able to unsubscribe: the
 *  permission is revoked from the browser's own UI, and the load that discovers
 *  that is a load on which nothing registered anything. Gating on the module
 *  variable meant the subscription outlived every revocation. */
async function existing(): Promise<ServiceWorkerRegistration | null> {
  if (registration) return registration;
  return (await navigator.serviceWorker.getRegistration(SW_PATH)) ?? null;
}

/** ready is the registration if this page has one and nothing if it does not.
 *  It never registers: the in-page notification path uses it to raise a
 *  notification through the worker on browsers that refuse the constructor, and
 *  a page that never subscribed should not acquire a worker by drawing one. */
export async function ready(): Promise<ServiceWorkerRegistration | null> {
  if (!registration) return null;
  try {
    return await registration;
  } catch {
    return null;
  }
}

/** Undoes the registration between tests. */
export function resetPush() {
  registration = null;
}

/** enable makes sure the daemon holds a current subscription for this browser.
 *
 *  It runs on every load rather than once, because the three things it
 *  reconciles all change behind its back: a browser can drop a subscription on
 *  its own, the daemon's keys can be replaced, and the store can be moved.
 *  Re-subscribing is cheap and the daemon stores by endpoint, so a repeat is a
 *  no-op row. */
export async function enable(): Promise<PushState> {
  if (!pushSupported()) return "unsupported";
  const { available, key } = await api<PushAvailability>("/api/chat/push");
  if (!available || !key) return "unavailable";

  const reg = await register();
  let sub = await reg.pushManager.getSubscription();
  if (sub && !subscribedTo(sub, key)) {
    // The daemon's keys changed - a new state directory, or a restored backup
    // without the old vapid.json. The old subscription can never be pushed to
    // again, and the browser refuses to re-subscribe with a different key
    // while it exists.
    await forget(sub);
    sub = null;
  }
  sub ??= await reg.pushManager.subscribe({
    // Required by every browser that implements this: a push may not be
    // silent, and the worker must show something for each one.
    userVisibleOnly: true,
    applicationServerKey: decodeKey(key),
  });
  await api<void>("/api/chat/push/subs", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(wire(sub)),
  });
  return "on";
}

/** disable drops this browser's subscription, in the browser and in the
 *  daemon. It is called when the operator has revoked the permission, so the
 *  daemon stops pushing to an endpoint whose notifications go nowhere. */
export async function disable(): Promise<void> {
  if (!pushSupported()) return;
  const reg = await existing();
  if (!reg) return;
  const sub = await reg.pushManager.getSubscription();
  if (sub) await forget(sub);
}

async function forget(sub: PushSubscription): Promise<void> {
  const endpoint = sub.endpoint;
  await sub.unsubscribe();
  // The endpoint travels in the body, not the URL: it is the capability to
  // notify this browser, and a URL is written to every log on the way.
  await api<void>("/api/chat/push/subs", {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ endpoint }),
  });
}

/** wire is the subscription in the shape internal/push stores. */
function wire(sub: PushSubscription): { endpoint: string; p256dh: string; auth: string } {
  const json = sub.toJSON();
  return {
    endpoint: sub.endpoint,
    p256dh: json.keys?.p256dh ?? "",
    auth: json.keys?.auth ?? "",
  };
}

/** subscribedTo reports whether an existing subscription was made against the
 *  key the daemon is signing with now. */
function subscribedTo(sub: PushSubscription, key: string): boolean {
  const applied = sub.options?.applicationServerKey;
  if (!applied) return false;
  return encodeKey(new Uint8Array(applied)) === key;
}

/** decodeKey turns the daemon's base64url key into the bytes subscribe wants.
 *  Browsers accept a string on some platforms and a BufferSource on all of
 *  them, so this always sends bytes. */
function decodeKey(key: string): ArrayBuffer {
  const padded = key.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(padded.padEnd(padded.length + ((4 - (padded.length % 4)) % 4), "="));
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
  return bytes.buffer;
}

function encodeKey(bytes: Uint8Array): string {
  let raw = "";
  for (const b of bytes) raw += String.fromCharCode(b);
  return btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** useWebPush keeps the daemon's idea of this browser in step with the
 *  browser's own.
 *
 *  It is driven by the permission the page already tracks rather than asking
 *  for one itself: a page that requests permission on load is one most browsers
 *  now refuse on the reader's behalf, and the existing button is the click that
 *  earns it.
 *
 *  It renders nothing and returns nothing. Push either reaches the phone or the
 *  tab title carries the count, and a badge for "notifications are only half
 *  on" would be a thing to explain rather than a thing to act on. */
export function useWebPush(permission: Permission): void {
  useEffect(() => {
    if (!pushSupported()) return;
    if (permission === "granted") {
      void enable().catch(() => {
        // The daemon is unreachable or the browser refused the subscription.
        // The next load tries again; nothing here is worth a screen.
      });
    } else if (permission === "denied") {
      // Revoked, or declined from the browser's own UI. Nothing here can ask
      // again, and a subscription that survives is one the daemon keeps
      // pushing into a void.
      void disable().catch(() => {});
    }
  }, [permission]);
}
