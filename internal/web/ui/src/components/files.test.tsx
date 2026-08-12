import { describe, expect, it } from "vitest";
import { MAX_ROWS, diff, shortPath, unified, uniqueLabels } from "./files";

describe("shortPath", () => {
  it("keeps the tail that identifies the file, without a stray slash", () => {
    expect(shortPath("/home/user/work/aigem/bin/uix-demo.txt")).toBe("bin/uix-demo.txt");
    expect(shortPath("/home/user/work/aigem/bin/uix-demo.txt", 4)).toBe(
      "work/aigem/bin/uix-demo.txt",
    );
    expect(shortPath("notes.md")).toBe("notes.md");
  });
});

describe("uniqueLabels", () => {
  it("lengthens only the labels that would otherwise collide", () => {
    const labels = uniqueLabels([
      "internal/web/ui/src/files.tsx",
      "internal/cli/ui/src/files.tsx",
      "docs/index.md",
    ]);

    expect(labels.get("docs/index.md")).toBe("docs/index.md");
    expect(labels.get("internal/web/ui/src/files.tsx")).toBe("web/ui/src/files.tsx");
    expect(labels.get("internal/cli/ui/src/files.tsx")).toBe("cli/ui/src/files.tsx");
  });

  it("leaves a single file alone", () => {
    expect(uniqueLabels(["a/b/c.go"]).get("a/b/c.go")).toBe("b/c.go");
  });
});

describe("diff", () => {
  it("numbers each side against its own file", () => {
    const rows = diff("alpha\nbeta\ngamma", "alpha\nBETA\ngamma\ndelta");

    expect(rows.map((r) => [r.kind, r.ln, r.rn])).toEqual([
      ["same", 1, 1],
      ["del", 2, undefined],
      ["add", undefined, 2],
      ["same", 3, 3],
      ["add", undefined, 4],
    ]);
  });

  it("does not invent a line for the newline the file ends with", () => {
    expect(diff("one\n", "one\ntwo\n").map((r) => [r.kind, r.ln, r.rn])).toEqual([
      ["same", 1, 1],
      ["add", undefined, 2],
    ]);
  });

  it("keeps a small change in a realistic large file local", () => {
    const before = Array.from({ length: MAX_ROWS }, (_, i) => `line ${i}`);
    const after = before.slice();
    after[2100] = "line 2100 changed";

    const rows = diff(before.join("\n"), after.join("\n"));

    expect(rows).toHaveLength(MAX_ROWS);
    expect(rows.filter((r) => r.kind !== "same")).toEqual([
      {
        kind: "replace",
        left: "line 2100",
        right: "line 2100 changed",
        ln: 2101,
        rn: 2101,
      },
    ]);
    expect(rows[2099]).toMatchObject({ kind: "same", ln: 2100, rn: 2100 });
    expect(rows[2101]).toMatchObject({ kind: "same", ln: 2102, rn: 2102 });
  });

  it("marks the side whose nonempty version lacks a trailing newline", () => {
    const missingOld = diff("a\nb", "a\nb\n");
    expect(missingOld.at(-1)).toEqual({
      left: "\\ No newline at end of file",
      kind: "del",
    });
    expect(missingOld.at(-1)?.ln).toBeUndefined();

    const missingNew = diff("a\nb\n", "a\nb");
    expect(missingNew.at(-1)).toEqual({
      right: "\\ No newline at end of file",
      kind: "add",
    });
    expect(missingNew.at(-1)?.rn).toBeUndefined();
  });

  it("does not add a newline marker to created, deleted, or empty files", () => {
    const hasMarker = (oldText: string, newText: string) => diff(oldText, newText).some(
      (r) => r.left?.startsWith("\\ No newline") || r.right?.startsWith("\\ No newline"),
    );
    expect(hasMarker("", "one")).toBe(false);
    expect(hasMarker("one", "")).toBe(false);
    expect(diff("", "")).toEqual([]);
  });

  it("treats a new file as all additions", () => {
    expect(diff("", "one\ntwo").map((r) => [r.kind, r.rn])).toEqual([
      ["add", 1],
      ["add", 2],
    ]);
  });
});

describe("unified", () => {
  it("splits a replacement into the old line then the new one", () => {
    expect(unified(diff("one\nkeep", "two\nkeep")).map((l) => [l.marker, l.text, l.ln, l.rn]))
      .toEqual([
        ["-", "one", 1, undefined],
        ["+", "two", undefined, 1],
        [" ", "keep", 2, 2],
      ]);
  });

  it("numbers each side in its own gutter", () => {
    // A deletion advances the old file's numbering and not the new file's, which
    // is the whole reason both gutters are shown rather than one.
    expect(unified(diff("a\nb\nc", "a\nc")).map((l) => [l.marker, l.text, l.ln, l.rn])).toEqual([
      [" ", "a", 1, 1],
      ["-", "b", 2, undefined],
      [" ", "c", 3, 2],
    ]);
  });

  it("leaves the newline marker unnumbered and unmarked", () => {
    const marker = unified(diff("one\ntwo", "one\ntwo\n")).at(-1);
    expect(marker).toEqual({
      marker: " ",
      text: "\\ No newline at end of file",
      tone: "meta",
    });
  });
});
