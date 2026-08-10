import { useCallback, useEffect, useState } from "react";
import { FileDiff, X } from "lucide-react";
import { api } from "@/lib/protocol";
import { Badge, Button, Spinner } from "./ui";
import { cn } from "@/lib/utils";

interface Artifact { path: string; created: boolean; old?: string; new?: string }

type Row = { left?: string; right?: string; kind: "same" | "add" | "del" };

/** A line diff over the whole file. The longest common subsequence is quadratic,
 *  so a large pair falls back to "replaced wholesale" rather than locking the
 *  tab up computing a prettier answer nobody waits for. */
function diff(oldText: string, newText: string): Row[] {
  const a = oldText.length ? oldText.split("\n") : [];
  const b = newText.length ? newText.split("\n") : [];
  if (a.length * b.length > 4_000_000) {
    return [
      ...a.map((l): Row => ({ left: l, kind: "del" })),
      ...b.map((l): Row => ({ right: l, kind: "add" })),
    ];
  }
  const n = a.length;
  const m = b.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const rows: Row[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) rows.push({ left: a[i], right: b[j++], kind: "same" }), i++;
    else if (lcs[i + 1][j] >= lcs[i][j + 1]) rows.push({ left: a[i++], kind: "del" });
    else rows.push({ right: b[j++], kind: "add" });
  }
  while (i < n) rows.push({ left: a[i++], kind: "del" });
  while (j < m) rows.push({ right: b[j++], kind: "add" });
  return rows;
}

function Side({ rows }: { rows: Row[] }) {
  return (
    <div className="grid grid-cols-2 gap-px overflow-x-auto bg-border font-mono text-[12px]">
      {rows.map((r, i) => (
        <>
          <div
            key={`l${i}`}
            className={cn(
              "whitespace-pre bg-panel px-2 py-0.5",
              r.kind === "del" && "bg-bad/15 text-bad",
              r.kind === "add" && "bg-panel-2",
            )}
          >
            {r.left ?? ""}
          </div>
          <div
            key={`r${i}`}
            className={cn(
              "whitespace-pre bg-panel px-2 py-0.5",
              r.kind === "add" && "bg-good/15 text-good",
              r.kind === "del" && "bg-panel-2",
            )}
          >
            {r.right ?? ""}
          </div>
        </>
      ))}
    </div>
  );
}

export function Files({ sessionID, onClose }: { sessionID: string; onClose: () => void }) {
  const [list, setList] = useState<Artifact[]>([]);
  const [open, setOpen] = useState<Artifact | null>(null);
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    setList(await api<Artifact[]>(`/api/sessions/${sessionID}/artifacts`));
  }, [sessionID]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const show = async (a: Artifact) => {
    setBusy(true);
    try {
      const got = await api<Artifact[]>(
        `/api/sessions/${sessionID}/artifacts?path=${encodeURIComponent(a.path)}`,
      );
      setOpen(got[0] ?? a);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center gap-2 border-b border-border bg-panel px-3 py-2">
        <FileDiff className="h-4 w-4 text-accent" />
        <span className="text-sm font-medium">Changed this session</span>
        <Badge className="ml-1">{list.length}</Badge>
        <Button variant="ghost" size="icon" className="ml-auto" onClick={onClose} title="Close">
          <X className="h-4 w-4" />
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        {list.length === 0 && (
          <p className="p-4 text-sm text-muted">Nothing has been written yet.</p>
        )}
        {list.map((a) => (
          <div key={a.path} className="border-b border-border">
            <button
              onClick={() => (open?.path === a.path ? setOpen(null) : void show(a))}
              className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-panel"
            >
              <span className="truncate font-mono text-[13px]">{a.path}</span>
              {a.created && <Badge className="border-good/40 text-good">new</Badge>}
              {busy && open?.path !== a.path && <Spinner className="ml-auto" />}
            </button>
            {open?.path === a.path && <Side rows={diff(open.old ?? "", open.new ?? "")} />}
          </div>
        ))}
      </div>
    </div>
  );
}
