import { useCallback, useEffect, useState } from "react";

/** The base title, restored whenever nothing is asking for the operator. */
const BASE = "aigem";

/** useTitleBadge puts the count of threads awaiting the operator in the tab
 *  title. It is the whole notification story for a tab that is merely in the
 *  background, and it costs nothing: no permission, no service worker, and it
 *  is right the moment the tab is looked at rather than when it was written. */
export function useTitleBadge(count: number) {
  useEffect(() => {
    document.title = count > 0 ? `(${count}) ${BASE}` : BASE;
    return () => {
      document.title = BASE;
    };
  }, [count]);
}

export type Permission = "default" | "granted" | "denied" | "unsupported";

function current(): Permission {
  if (typeof Notification === "undefined") return "unsupported";
  return Notification.permission;
}

/** useNotifications raises a system notification when a thread starts asking
 *  for the operator, and nothing else. A bot finishing a turn is not a reason
 *  to interrupt someone; a bot blocked on a decision is.
 *
 *  Permission is requested from a click and never on load: a page that asks the
 *  moment it opens is a page most browsers now refuse on the reader's behalf.
 *  Until it is granted the tab title carries the same count, so nothing is lost
 *  by declining - which is what makes declining reasonable. */
export function useNotifications(
  alerts: string[],
  titleOf: (thread: string) => string,
  drain: () => void,
) {
  const [permission, setPermission] = useState<Permission>(current);

  const ask = useCallback(async () => {
    if (typeof Notification === "undefined") return;
    setPermission(await Notification.requestPermission());
  }, []);

  useEffect(() => {
    if (alerts.length === 0) return;
    try {
      if (permission === "granted" && document.visibilityState !== "visible") {
        for (const thread of alerts) {
          // Tagged by thread, so a thread that asks twice replaces its own
          // notification instead of stacking a second one behind it.
          new Notification(titleOf(thread), { body: "needs you", tag: thread });
        }
      }
    } catch {
      // Chrome on Android grants the permission and then throws from the
      // constructor, demanding ServiceWorkerRegistration.showNotification
      // instead - which arrives with Web Push, a later stage. Unguarded, that
      // throw escapes the effect and React unmounts the whole screen, so a
      // phone loses the inbox the moment a bot asks it something. Failing to
      // notify is not a reason to stop showing the conversation; the tab title
      // carries the same count either way.
    } finally {
      // Drained whatever happened, including a failure: an alert kept back
      // would fire later for a thread answered an hour ago.
      drain();
    }
  }, [alerts, permission, titleOf, drain]);

  return { permission, ask };
}
