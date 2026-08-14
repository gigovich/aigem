import { useCallback, useEffect, useState } from "react";
import { ready } from "./push";

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

/** raise shows one notification from the open page.
 *
 *  Through the service worker when there is one, and through the constructor
 *  otherwise. Chrome on Android grants the permission and then throws from the
 *  constructor, demanding ServiceWorkerRegistration.showNotification instead -
 *  which is exactly what a page that has subscribed to push already has. */
function raise(title: string, tag: string) {
  // data.url matters even here. Shown through the worker, this notification's
  // click is handled by the worker too, and a click with nothing to open falls
  // back to the inbox - which navigates the window away from whatever the
  // operator had open, to the one screen the notification was not about.
  const options = { body: "needs you", tag, data: { url: `/chat/${tag}` } };
  void ready().then((reg) => {
    if (reg) {
      void reg.showNotification(title, options);
      return;
    }
    try {
      // Its click is this page's to handle: a notification raised by the
      // constructor never reaches the service worker, so without this, tapping
      // it does nothing at all.
      const shown = new Notification(title, options);
      shown.onclick = () => {
        window.focus();
        window.location.assign(options.data.url);
      };
    } catch {
      // No worker and no constructor: the tab title still carries the count.
    }
  });
}

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
          // notification instead of stacking a second one behind it - and the
          // same tag the pushed notification uses, so the two cannot both be on
          // screen for one conversation.
          raise(titleOf(thread), thread);
        }
      }
    } finally {
      // Drained whatever happened, including a failure: an alert kept back
      // would fire later for a thread answered an hour ago. raise() swallows
      // its own failures, so this runs either way.
      drain();
    }
  }, [alerts, permission, titleOf, drain]);

  return { permission, ask };
}
