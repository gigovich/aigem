/**
 * A WebSocket the tests drive by hand.
 *
 * jsdom's own would try to connect, so every socket test stubs the global with
 * this: it records what was sent, and the test decides when the connection
 * opens, what arrives and when it ends. That is the only way to assert the
 * things that matter here - a gap in the revisions, a desync, a reconnect -
 * because all three are about the order events reach the page in.
 */

export class FakeSocket {
  static instances: FakeSocket[] = []

  static get last(): FakeSocket {
    const s = FakeSocket.instances[FakeSocket.instances.length - 1]
    if (!s) throw new Error('no socket was opened')
    return s
  }

  static reset() {
    FakeSocket.instances = []
  }

  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  readonly url: string
  readonly sent: string[] = []
  readyState = FakeSocket.CONNECTING
  onopen: (() => void) | null = null
  onmessage: ((e: { data: unknown }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null

  constructor(url: string) {
    this.url = url
    FakeSocket.instances.push(this)
  }

  send(data: string) {
    this.sent.push(data)
  }

  close() {
    if (this.readyState === FakeSocket.CLOSED) return
    this.readyState = FakeSocket.CLOSED
    this.onclose?.()
  }

  /* The test's side of the wire. */

  open() {
    this.readyState = FakeSocket.OPEN
    this.onopen?.()
  }

  deliver(frame: unknown) {
    this.onmessage?.({ data: typeof frame === 'string' ? frame : JSON.stringify(frame) })
  }

  /** The daemon hanging up, as opposed to the page closing the socket. */
  drop() {
    this.readyState = FakeSocket.CLOSED
    this.onclose?.()
  }
}

export function installFakeSocket(): void {
  FakeSocket.reset()
  vi.stubGlobal('WebSocket', FakeSocket)
}
