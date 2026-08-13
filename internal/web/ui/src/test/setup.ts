import "@testing-library/jest-dom/vitest";
import { beforeEach } from "vitest";
import { resetAuth } from "@/lib/protocol";

// The token-for-cookie exchange happens once per page, and a page is a module
// here: without this, the first test's answer decides how every later test in
// the file authenticates, including tests that stand up a different daemon.
beforeEach(() => resetAuth());

// jsdom implements no layout, so it has no scrollIntoView at all. Every stream
// in this UI follows its own tail with one, and a component that called it
// would throw here for a reason that says nothing about the component. This is
// the environment catching up with the browser, not a stand-in for behaviour:
// nothing asserts on scrolling, because jsdom could not tell the truth about it.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}
