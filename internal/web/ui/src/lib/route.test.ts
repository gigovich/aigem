import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { FLEET_PATH, modeOf, replaceMode, threadOf, useFleetScreen, useMode, useThread } from "./route";

beforeEach(() => window.history.replaceState({}, "", "/"));
afterEach(cleanup);

describe("modeOf", () => {
  it("reads the screen out of the path", () => {
    expect(modeOf("/")).toBe("sessions");
    expect(modeOf("/chat")).toBe("chat");
    expect(modeOf("/chat/t_945dde0c47180ba8")).toBe("chat");
    // Not a prefix match: a path that merely starts with the same letters is a
    // different screen, and treating it as this one is how "/chatter" becomes
    // the bot inbox.
    expect(modeOf("/chatter")).toBe("sessions");
  });
});

describe("useMode", () => {
  it("starts from the URL the page was opened at", () => {
    window.history.replaceState({}, "", "/chat");
    const { result } = renderHook(() => useMode());
    expect(result.current[0]).toBe("chat");
  });

  it("pushes a history entry the back button can leave", () => {
    const { result } = renderHook(() => useMode());
    act(() => result.current[1]("chat"));

    expect(window.location.pathname).toBe("/chat");
    expect(result.current[0]).toBe("chat");
  });

  it("follows the back button", () => {
    const { result } = renderHook(() => useMode());
    act(() => result.current[1]("chat"));

    window.history.replaceState({}, "", "/");
    act(() => window.dispatchEvent(new PopStateEvent("popstate")));

    expect(result.current[0]).toBe("sessions");
  });

  it("carries the query string across a push", () => {
    // Empty by the time this runs - protocol.ts strips the token out on first
    // read - but a push that dropped it would silently change what a link
    // copied out of the address bar is worth.
    window.history.replaceState({}, "", "/?since=4");
    const { result } = renderHook(() => useMode());
    act(() => result.current[1]("chat"));

    expect(window.location.search).toBe("?since=4");
  });

  it("does not stack an entry for the screen already open", () => {
    const { result } = renderHook(() => useMode());
    const depth = window.history.length;
    act(() => result.current[1]("sessions"));

    expect(window.history.length).toBe(depth);
  });
});

describe("replaceMode", () => {
  it("corrects the URL without leaving the wrong screen one back button away", () => {
    window.history.replaceState({}, "", "/chat");
    const depth = window.history.length;
    replaceMode("sessions");

    expect(window.location.pathname).toBe("/");
    expect(window.history.length).toBe(depth);
  });
});

describe("useThread", () => {
  it("reads the thread the URL names", () => {
    window.history.replaceState({}, "", "/chat/t_945dde0c47180ba8");
    const { result } = renderHook(() => useThread());
    expect(result.current.thread).toBe("t_945dde0c47180ba8");
  });

  it("treats bare /chat as the inbox", () => {
    window.history.replaceState({}, "", "/chat");
    const { result } = renderHook(() => useThread());
    expect(result.current.thread).toBeNull();
  });

  it("survives a malformed escape instead of blanking the page", () => {
    // decodeURIComponent throws on "/chat/50%", and this runs during render
    // with no error boundary above it - so an unguarded decode turned a
    // mistyped URL into an empty application.
    expect(() => threadOf("/chat/50%")).not.toThrow();
    expect(threadOf("/chat/50%")).toBe("50%");
  });

  it("pushes an entry, then pops it rather than pushing another", () => {
    window.history.replaceState({}, "", "/chat");
    const { result } = renderHook(() => useThread());

    act(() => result.current.open("t_1"));
    expect(window.location.pathname).toBe("/chat/t_1");
    const depth = window.history.length;

    // Back must pop. Pushing "/chat" instead would leave the thread one
    // hardware-back away, so the phone's back button walks into the thread the
    // operator just left.
    act(() => result.current.close());
    expect(window.history.length).toBe(depth);
  });

  it("rewrites in place when the operator arrived on the link itself", () => {
    // Nothing of ours to pop: popping here would leave the application.
    window.history.replaceState({}, "", "/chat/t_1");
    const { result } = renderHook(() => useThread());
    const depth = window.history.length;

    act(() => result.current.close());

    expect(window.location.pathname).toBe("/chat");
    expect(result.current.thread).toBeNull();
    expect(window.history.length).toBe(depth);
  });

  it("does not stack an entry for the thread already open", () => {
    window.history.replaceState({}, "", "/chat/t_1");
    const { result } = renderHook(() => useThread());
    const depth = window.history.length;

    act(() => result.current.open("t_1"));
    expect(window.history.length).toBe(depth);
  });
});

describe("useFleetScreen", () => {
  it("reads the roster out of the URL", () => {
    window.history.replaceState({}, "", FLEET_PATH);
    const { result } = renderHook(() => useFleetScreen());
    expect(result.current.fleet).toBe(true);
  });

  it("is not a thread", () => {
    // Thread ids are `t_` + hex, so this segment can never be one - but the
    // decode below it would happily return "fleet" as a thread id.
    expect(threadOf(FLEET_PATH)).toBeNull();
  });

  it("pushes an entry, then pops it rather than pushing another", () => {
    window.history.replaceState({}, "", "/chat");
    const { result } = renderHook(() => useFleetScreen());

    act(() => result.current.open());
    expect(window.location.pathname).toBe(FLEET_PATH);
    const depth = window.history.length;

    act(() => result.current.close());
    expect(window.history.length).toBe(depth);
  });

  it("rewrites in place when the operator arrived on the link itself", () => {
    window.history.replaceState({}, "", FLEET_PATH);
    const { result } = renderHook(() => useFleetScreen());

    act(() => result.current.close());

    expect(window.location.pathname).toBe("/chat");
    expect(result.current.fleet).toBe(false);
  });

  // The bug the shared path store exists to prevent: pushState fires no event,
  // so two hooks each holding their own copy of the URL only agree until one of
  // them navigates. Opening the roster over a thread left the thread hook still
  // naming the thread, and the screen drew both.
  it("closes the open thread, because both hooks read one URL", () => {
    window.history.replaceState({}, "", "/chat/t_1");
    const { result } = renderHook(() => ({ thread: useThread(), roster: useFleetScreen() }));
    expect(result.current.thread.thread).toBe("t_1");

    act(() => result.current.roster.open());

    expect(result.current.roster.fleet).toBe(true);
    expect(result.current.thread.thread).toBeNull();
  });

  it("and the other way round", () => {
    window.history.replaceState({}, "", FLEET_PATH);
    const { result } = renderHook(() => ({ thread: useThread(), roster: useFleetScreen() }));

    act(() => result.current.thread.open("t_1"));

    expect(result.current.thread.thread).toBe("t_1");
    expect(result.current.roster.fleet).toBe(false);
  });
});
