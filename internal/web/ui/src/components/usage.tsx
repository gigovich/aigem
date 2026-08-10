import { useEffect, useState } from "react";
import { Gauge } from "lucide-react";
import { api } from "@/lib/protocol";
import { Badge } from "./ui";

interface Window { label?: string; used_pct?: number; resets_at?: string }
interface Limits { plan?: string; credits?: string; windows?: Window[]; observed_at?: string }
interface Usage { provider: string; limits: Limits }

/** Quota readings taken from real responses and persisted as they arrive, so
 *  showing them continuously costs nothing. A provider that reports none is
 *  simply absent rather than shown as zero. */
export function Spend() {
  const [rows, setRows] = useState<Usage[]>([]);
  useEffect(() => {
    void api<Usage[]>("/api/usage").then(setRows).catch(() => setRows([]));
  }, []);
  if (rows.length === 0) return null;
  return (
    <div className="shrink-0 border-b border-border bg-panel-2 px-3 py-2">
      <div className="mx-auto flex max-w-3xl flex-wrap items-center gap-3">
        <Gauge className="h-4 w-4 text-accent" />
        {rows.map((r) => (
          <div key={r.provider} className="flex items-center gap-1.5">
            <Badge>{r.provider}</Badge>
            {r.limits.plan && <span className="text-[12px] text-muted">{r.limits.plan}</span>}
            {(r.limits.windows ?? []).map((w, i) => (
              <span key={i} className="text-[12px] text-muted">
                {w.label ?? "window"} {Math.round(w.used_pct ?? 0)}%
              </span>
            ))}
            {r.limits.credits && (
              <span className="text-[12px] text-muted">{r.limits.credits}</span>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
