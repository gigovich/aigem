import { describe, expect, it } from "vitest";
import { chatReducer, countsOf, emptyChat, inboxOf, type ChatState } from "./chat";
import type { Frame, Message, ThreadState, ThreadView } from "./chatprotocol";

function view(id: string, over: Partial<ThreadView> = {}): ThreadView {
  return {
    id,
    title: id,
    created: "2026-08-13T09:00:00Z",
    created_by: "human:operator",
    last_seq: 1,
    last_at: "2026-08-13T09:00:00Z",
    state: "idle",
    participants: ["human:operator", "bot:amiran"],
    unread: 0,
    working: false,
    ...over,
  };
}

function message(seq: number, thread: string, over: Partial<Message> = {}): Message {
  return {
    seq,
    thread,
    author: "bot:amiran",
    body: `message ${seq}`,
    kind: "message",
    created: "2026-08-13T09:00:00Z",
    ...over,
  };
}

function apply(state: ChatState, ...frames: Frame[]): ChatState {
  return frames.reduce((s, f) => chatReducer(s, { t: "frame", f }), state);
}

describe("chatReducer", () => {
  it("keeps a message that arrives while its first page is still in flight", () => {
    // The window this closes is the one the daemon's own socket attach had:
    // between declaring interest and the answer landing, anything said belongs
    // to a thread the client has not admitted to holding yet.
    let s = chatReducer(emptyChat, { t: "opening", thread: "t_1" });
    s = apply(s, { seq: 9, stream: "message", thread: "t_1", msg: message(9, "t_1") });
    s = chatReducer(s, {
      t: "page",
      thread: "t_1",
      page: { items: [message(8, "t_1"), message(7, "t_1")] },
    });

    expect(s.messages.t_1.items.map((m) => m.seq)).toEqual([7, 8, 9]);
    expect(s.messages.t_1.loaded).toBe(true);
  });

  it("does not hold, or re-render for, messages in a thread nobody opened", () => {
    // The inbox row stays current regardless: a thread frame rides with every
    // message. Returning the same state - identity and all - is what keeps a
    // fleet mid-turn from re-rendering the screen once per message nobody is
    // reading. The resume point lives in the socket's own ref, not here.
    const s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1")] });
    const after = apply(s, { seq: 4, stream: "message", thread: "t_9", msg: message(4, "t_9") });

    expect(after).toBe(s);
    expect(after.messages.t_9).toBeUndefined();
  });

  it("delivers the same message twice without duplicating it", () => {
    let s = chatReducer(emptyChat, { t: "opening", thread: "t_1" });
    s = chatReducer(s, { t: "page", thread: "t_1", page: { items: [message(5, "t_1")] } });
    s = apply(s, { seq: 5, stream: "message", thread: "t_1", msg: message(5, "t_1") });

    expect(s.messages.t_1.items).toHaveLength(1);
  });

  it("carries the cursor and the more flag rather than inferring them", () => {
    let s = chatReducer(emptyChat, { t: "opening", thread: "t_1" });
    s = chatReducer(s, {
      t: "page",
      thread: "t_1",
      page: { items: [message(9, "t_1"), message(8, "t_1")], cursor: 8, more: true },
    });
    expect(s.messages.t_1).toMatchObject({ cursor: 8, more: true });

    s = chatReducer(s, {
      t: "page",
      thread: "t_1",
      page: { items: [message(7, "t_1"), message(6, "t_1")] },
    });
    // The older page ended the thread, so there is nothing left to ask for.
    expect(s.messages.t_1).toMatchObject({ more: false });
    expect(s.messages.t_1.items.map((m) => m.seq)).toEqual([6, 7, 8, 9]);
  });

  it("does not wind the cursor forward when a thread is re-opened", () => {
    // Re-opening refetches page one. Adopting its cursor would put the "Older
    // messages" button back for messages already on screen, and the next two
    // clicks would refetch pages the client is already holding.
    let s = chatReducer(emptyChat, { t: "opening", thread: "t_1" });
    s = chatReducer(s, {
      t: "page",
      thread: "t_1",
      page: { items: [message(9, "t_1"), message(8, "t_1")], cursor: 8, more: true },
    });
    s = chatReducer(s, {
      t: "page",
      thread: "t_1",
      page: { items: [message(7, "t_1"), message(6, "t_1")] },
    });
    expect(s.messages.t_1).toMatchObject({ more: false });

    // The re-open: the same first page, arriving again.
    s = chatReducer(s, {
      t: "page",
      thread: "t_1",
      page: { items: [message(9, "t_1"), message(8, "t_1")], cursor: 8, more: true },
    });

    expect(s.messages.t_1).toMatchObject({ more: false, cursor: 0 });
    expect(s.messages.t_1.items.map((m) => m.seq)).toEqual([6, 7, 8, 9]);
  });

  it("does not let a stale inbox snapshot undo a rename", () => {
    // A rename moves the thread's changed_seq and not its last_seq, so a client
    // comparing last_seq alone would take the older HTTP answer as newer.
    let s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1", { title: "old" })] });
    s = apply(s, {
      seq: 40,
      stream: "thread",
      thread: "t_1",
      thr: view("t_1", { title: "renamed", last_seq: 1 }),
    });
    s = chatReducer(s, { t: "inbox", views: [view("t_1", { title: "old", last_seq: 1 })] });

    expect(inboxOf(s)[0].title).toBe("renamed");
  });

  it("drops a thread on its tombstone", () => {
    let s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1")] });
    s = chatReducer(s, { t: "opening", thread: "t_1" });
    s = apply(s, { seq: 12, stream: "thread", thread: "t_1" });

    expect(inboxOf(s)).toEqual([]);
    expect(s.messages.t_1).toBeUndefined();
  });

  it("does not let an inbox response in flight resurrect a deleted thread", () => {
    // The snapshot was read before the delete committed, so it lists a thread
    // that is gone. Put back in the rail, it answers 404 on the first click.
    let s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1")] });
    s = apply(s, { seq: 12, stream: "thread", thread: "t_1" });
    s = chatReducer(s, { t: "inbox", views: [view("t_1")] });

    expect(inboxOf(s)).toEqual([]);
  });

  it("does not re-render for timeline events nothing draws yet", () => {
    // One bot turn is hundreds of these. Until something renders them, a new
    // state object per event is a re-render of the whole screen per step of
    // work no one is looking at. The socket's own cursor still advances.
    const s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1")] });
    expect(apply(s, { seq: 77, stream: "event", thread: "t_1" })).toBe(s);
  });

  it("rewinds the resume point to where a desync says it was", () => {
    let s = apply(emptyChat, {
      seq: 90,
      stream: "thread",
      thread: "t_1",
      thr: view("t_1", { last_seq: 90 }),
    });
    expect(s.lastSeq).toBe(90);

    s = apply(s, { seq: 0, stream: "desync", from: 12 });
    expect(s.lastSeq).toBe(12);
  });

  it("treats a truncated backlog as a cursor to resume from, not a loss", () => {
    let s = chatReducer(emptyChat, { t: "opening", thread: "t_1" });
    s = apply(s, { seq: 3, stream: "message", thread: "t_1", msg: message(3, "t_1") });
    s = apply(s, { seq: 0, stream: "truncated", from: 3 });

    expect(s.lastSeq).toBe(3);
    // Unlike a desync, nothing already delivered is suspect: the backlog stops
    // short, it does not go missing.
    expect(s.messages.t_1.items.map((m) => m.seq)).toEqual([3]);
  });

  it("orders the inbox by activity and keeps the archived out of it", () => {
    const s = chatReducer(emptyChat, {
      t: "inbox",
      views: [
        view("t_old", { last_seq: 2 }),
        view("t_new", { last_seq: 30 }),
        view("t_gone", { last_seq: 90, archived: true }),
      ],
    });

    expect(inboxOf(s).map((v) => v.id)).toEqual(["t_new", "t_old"]);
    expect(inboxOf(s, undefined, true).map((v) => v.id)).toEqual(["t_gone"]);
  });

  it("counts the filter chips from the rows they filter", () => {
    const s = chatReducer(emptyChat, {
      t: "inbox",
      views: [
        view("t_1", { state: "needs_you" }),
        view("t_2", { state: "needs_you" }),
        view("t_3", { state: "working" }),
        view("t_4", { state: "idle", archived: true }),
      ],
    });

    expect(countsOf(s)).toEqual({ needs_you: 2, working: 1, waiting: 0, idle: 0 });
    expect(inboxOf(s, "needs_you")).toHaveLength(2);
  });
});

describe("needs_you alerts", () => {
  const asking = (id: string, seq: number): Frame => ({
    seq,
    stream: "thread",
    thread: id,
    thr: view(id, { state: "needs_you" as ThreadState, last_seq: seq }),
  });

  it("raises one when a thread on screen starts asking", () => {
    let s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1", { state: "working" })] });
    s = apply(s, asking("t_1", 20));

    expect(s.alerts).toEqual(["t_1"]);
    expect(chatReducer(s, { t: "alerted" }).alerts).toEqual([]);
  });

  it("stays quiet for a thread that was already asking when the page loaded", () => {
    // The replay a fresh load gets would otherwise raise one notification per
    // pending thread, which is how the notification stops meaning anything.
    const s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1", { state: "needs_you" })] });
    expect(s.alerts).toEqual([]);
  });

  it("stays quiet when a thread that is already asking is described again", () => {
    let s = chatReducer(emptyChat, { t: "inbox", views: [view("t_1", { state: "needs_you" })] });
    s = apply(s, asking("t_1", 20), asking("t_1", 21));

    expect(s.alerts).toEqual([]);
  });
});
