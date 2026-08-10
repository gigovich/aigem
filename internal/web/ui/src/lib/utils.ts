import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** cn merges class lists so a caller's override wins over a component default. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/** The one argument worth reading at a glance: the command, the path, the
 *  pattern. Shared by the tool card and the approval, which must name the same
 *  thing - approving "bash" without its command is approving nothing. */
export function argSummary(args: unknown): string {
  if (args == null) return "";
  if (typeof args === "string") return args;
  const o = args as Record<string, unknown>;
  for (const k of ["cmd", "command", "path", "pattern", "query", "url", "text"]) {
    if (typeof o[k] === "string") return o[k] as string;
  }
  return JSON.stringify(args);
}

function clip(text: string, max = 400): string {
  return text.length > max ? `${text.slice(0, max)}\n...` : text;
}

/** What a confirmation is actually granting. A path alone is enough for reading
 *  a file and nowhere near enough for replacing one: `write_file` hands over the
 *  whole contents and `edit_file` can rewrite every occurrence, and neither says
 *  so in its path. */
export function approvalDetail(tool: string, args: unknown): string {
  const o = (args ?? {}) as Record<string, unknown>;
  const str = (k: string) => (typeof o[k] === "string" ? (o[k] as string) : "");
  const path = str("path");
  if (tool === "write_file" && path) {
    return `${path}\n\n${clip(str("content"))}`;
  }
  if (tool === "edit_file" && path) {
    const scope = o.replace_all === true ? "  (every occurrence)" : "";
    return `${path}${scope}\n\n- ${clip(str("old_string"))}\n+ ${clip(str("new_string"))}`;
  }
  return argSummary(args);
}
