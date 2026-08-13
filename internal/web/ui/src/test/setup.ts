import "@testing-library/jest-dom/vitest";

// jsdom implements no layout, so it has no scrollIntoView at all. Every stream
// in this UI follows its own tail with one, and a component that called it
// would throw here for a reason that says nothing about the component. This is
// the environment catching up with the browser, not a stand-in for behaviour:
// nothing asserts on scrolling, because jsdom could not tell the truth about it.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}
