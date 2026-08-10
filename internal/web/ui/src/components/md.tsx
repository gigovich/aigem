import { useMemo } from "react";
import { marked } from "marked";
import DOMPurify from "dompurify";

marked.setOptions({ gfm: true, breaks: true });

/** Markdown from a model is untrusted input rendered as HTML, so it is
 *  sanitised rather than trusted. Links open in a new tab and carry noopener,
 *  which the sanitiser strips unless it is added after. */
export function Markdown({ text }: { text: string }) {
  const html = useMemo(() => {
    const raw = marked.parse(text, { async: false });
    return DOMPurify.sanitize(raw, { ADD_ATTR: ["target", "rel"] });
  }, [text]);
  return (
    <div
      className="md text-[15px] leading-relaxed"
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
}

DOMPurify.addHook("afterSanitizeAttributes", (node) => {
  if (node.tagName === "A") {
    node.setAttribute("target", "_blank");
    node.setAttribute("rel", "noopener noreferrer");
  }
});
