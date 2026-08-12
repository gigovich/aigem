import { useCallback, useEffect, useState } from "react";
import { KeyRound, X } from "lucide-react";
import { api } from "@/lib/protocol";
import { Badge, Button, RunDot } from "./ui";

interface ModelView {
  ref: string;
  provider: string;
  authenticated: boolean;
  needs_auth: boolean;
  active: boolean;
}

interface Flow {
  id: string;
  url: string;
  code?: string;
  provider: string;
  paste: boolean;
  state: "pending" | "done" | "failed";
  error?: string;
}

/** Providers with a browser login. An API key is pasted once on the command
 *  line; walking someone through one here would be ceremony around a text
 *  field. */
const INTERACTIVE = new Set(["openai", "xai"]);

export function Login({ onClose }: { onClose: () => void }) {
  const [models, setModels] = useState<ModelView[]>([]);
  const [flow, setFlow] = useState<Flow | null>(null);
  const [pasted, setPasted] = useState("");
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setModels(await api<ModelView[]>("/api/models"));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    // refresh only sets state after its await, so this is a subscription to the
    // daemon rather than the cascading render the rule is guarding against.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void refresh();
  }, [refresh]);

  // Poll while a login is open. The daemon is waiting on the provider, so this
  // is the only way to learn it finished.
  useEffect(() => {
    if (!flow || flow.state !== "pending") return;
    const t = window.setInterval(async () => {
      try {
        const next = await api<Flow>(`/api/auth/login/${flow.id}`);
        setFlow(next);
        if (next.state === "done") void refresh();
      } catch {
        /* a poll that fails is retried by the next tick */
      }
    }, 1500);
    return () => window.clearInterval(t);
  }, [flow, refresh]);

  const begin = async (provider: string) => {
    setError(null);
    setPasted("");
    try {
      setFlow(await api<Flow>(`/api/auth/login/${provider}`, { method: "POST" }));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const paste = async () => {
    if (!flow) return;
    try {
      setFlow(await api<Flow>(`/api/auth/login/${flow.id}/paste`, {
        method: "POST",
        body: pasted.trim(),
      }));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  // The interactive providers are always offered, even when the model list is
  // empty - which is exactly the state a first-time user is in, and the one
  // where they most need somewhere to log in.
  const providers = Array.from(
    new Set([...INTERACTIVE, ...models.map((m) => m.provider).filter(Boolean)]),
  );

  return (
    <div className="shrink-0 border-b border-line bg-raised">
      <div className="px-4 py-3">
        <div className="flex items-center gap-2">
          <KeyRound className="h-4 w-4 text-accent" />
          <span className="text-[15px] font-medium">Providers</span>
          <Button variant="ghost" size="icon" className="ml-auto" onClick={onClose} title="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>

        {error && <p className="mt-2 text-[13px] text-bad">{error}</p>}

        <div className="mt-2 flex flex-wrap gap-2">
          {providers.map((p) => {
            const ok = models.some((m) => m.provider === p && m.authenticated);
            return (
              <div key={p} className="flex items-center gap-1.5">
                <Badge className={ok ? "border-good/40 text-good" : undefined}>{p}</Badge>
                {INTERACTIVE.has(p) ? (
                  <Button size="sm" variant="outline" onClick={() => void begin(p)}>
                    {ok ? "Re-authenticate" : "Log in"}
                  </Button>
                ) : (
                  !ok && <span className="text-[12px] text-muted">needs an API key</span>
                )}
              </div>
            );
          })}
        </div>

        {flow && (
          <div className="mt-3 rounded-lg border border-line bg-panel p-3">
            {flow.state === "done" ? (
              <p className="text-[13px] text-good">Signed in to {flow.provider}.</p>
            ) : flow.state === "failed" ? (
              <p className="text-[13px] text-bad">{flow.error ?? "login failed"}</p>
            ) : (
              <>
                <p className="text-[13px] text-muted">
                  Open this and approve access:
                </p>
                <a
                  href={flow.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="mt-1 block break-all text-[13px] text-accent underline"
                >
                  {flow.url}
                </a>
                {flow.code && (
                  <p className="mt-2 text-[13px]">
                    Code: <span className="font-mono text-fg">{flow.code}</span>
                  </p>
                )}
                <div className="mt-2 flex items-center gap-2 text-[12px] text-muted">
                  <RunDot /> waiting for approval
                </div>
                {flow.paste && (
                  <div className="mt-3 border-t border-line pt-3">
                    {/* The provider sends the browser back to that browser's own
                        localhost. On this machine it arrives by itself; from a
                        phone it never can, so it comes back by hand. */}
                    <p className="text-[12px] text-muted">
                      On another device? Paste the URL it redirected to:
                    </p>
                    <div className="mt-1 flex gap-2">
                      <input
                        value={pasted}
                        onChange={(e) => setPasted(e.target.value)}
                        placeholder="http://localhost:1455/auth/callback?code=..."
                        className="min-w-0 flex-1 rounded-md border border-line bg-raised px-2 py-1.5 text-[13px] outline-none placeholder:text-muted focus:border-accent/60"
                      />
                      <Button size="sm" onClick={() => void paste()} disabled={!pasted.trim()}>
                        Submit
                      </Button>
                    </div>
                  </div>
                )}
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
