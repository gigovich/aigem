import { vi } from "vitest";
import type { Event, Kind } from "@/lib/protocol";

/** What the daemon actually writes to the socket: session events, plus the
 *  client_error rejection that internal/web/ws.go keeps out of the Event kinds
 *  on purpose - one client's bad request did not happen in the conversation. */
export type Frame =
  | (Partial<Omit<Event, "kind">> & { kind: Kind })
  | { kind: "client_error"; op?: string; error: string };

/** A stand-in for the browser WebSocket. useSession's whole job is deciding
 *  what to resume from, and that decision is only visible in the URL it dials,
 *  so the tests need the sockets it opened, not a real connection. */
export class FakeSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static opened: FakeSocket[] = [];

  readyState = FakeSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((m: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(readonly url: string) {
    FakeSocket.opened.push(this);
  }

  get since(): number {
    return Number(new URL(this.url).searchParams.get("since"));
  }

  open() {
    this.readyState = FakeSocket.OPEN;
    this.onopen?.();
  }

  deliver(frame: Frame) {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }

  send(data: string) {
    this.sent.push(data);
  }

  close() {
    if (this.readyState === FakeSocket.CLOSED) return;
    this.readyState = FakeSocket.CLOSED;
    this.onclose?.();
  }
}

export function installFakeSocket() {
  FakeSocket.opened = [];
  vi.stubGlobal("WebSocket", FakeSocket);
  return FakeSocket;
}
