import { describe, expect, it } from "vitest";
import { approvalDetail, argSummary } from "./utils";

describe("argSummary", () => {
  it("prefers the command over any other field", () => {
    expect(argSummary({ text: "note", cmd: "rm -rf build" })).toBe("rm -rf build");
    expect(argSummary({ path: "a.go", pattern: "func" })).toBe("a.go");
  });

  it("passes a string through and falls back to the whole object", () => {
    expect(argSummary("ls -la")).toBe("ls -la");
    expect(argSummary({ depth: 2 })).toBe('{"depth":2}');
    expect(argSummary(null)).toBe("");
  });
});

describe("approvalDetail", () => {
  it("shows what a write replaces the file with, not just its name", () => {
    const detail = approvalDetail("write_file", { path: "a.go", content: "package main" });

    expect(detail).toContain("a.go");
    expect(detail).toContain("package main");
  });

  it("names the scope of an edit, because every occurrence is a different act", () => {
    const one = approvalDetail("edit_file", { path: "a.go", old_string: "x", new_string: "y" });
    const all = approvalDetail("edit_file", {
      path: "a.go", old_string: "x", new_string: "y", replace_all: true,
    });

    expect(one).not.toContain("every occurrence");
    expect(all).toContain("every occurrence");
    expect(all).toContain("- x");
    expect(all).toContain("+ y");
  });

  it("clips content that would push the buttons off the screen", () => {
    const detail = approvalDetail("write_file", { path: "a.go", content: "x".repeat(5000) });

    expect(detail.length).toBeLessThan(600);
    expect(detail).toContain("...");
  });

  it("falls back to the plain summary for every other tool", () => {
    expect(approvalDetail("bash", { cmd: "ls" })).toBe("ls");
  });
});
