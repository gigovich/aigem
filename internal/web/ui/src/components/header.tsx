import { useState } from "react";
import { FileDiff, KeyRound, ListChecks, MessagesSquare, MoreHorizontal, WifiOff } from "lucide-react";
import { Badge, Button } from "./ui";
import { cn } from "@/lib/utils";

interface HeaderProps {
  conversationCount: number;
  title: string;
  model: string;
  tokens: number;
  ctx: number;
  clientCount: number;
  connected: boolean;
  planDone?: number;
  planTotal?: number;
  navOpen: boolean;
  railOpen: boolean;
  onToggleConversations: () => void;
  onToggleFiles: () => void;
  onToggleProviders: () => void;
}

function SessionBadges({
  model,
  tokens,
  ctx,
  clientCount,
  connected,
}: Pick<HeaderProps, "model" | "tokens" | "ctx" | "clientCount" | "connected">) {
  return (
    <>
      {model && <Badge className="max-w-40 truncate">{model}</Badge>}
      {tokens > 0 && (
        <Badge>
          {Math.round(tokens / 1000)}k
          {ctx > 0 && ` / ${Math.round(ctx / 1000)}k`} ctx
        </Badge>
      )}
      {clientCount > 1 && <Badge>{clientCount} attached</Badge>}
      {!connected && (
        <Badge className="border-warn/40 text-warn">
          <WifiOff className="mr-1 h-3 w-3" /> reconnecting
        </Badge>
      )}
    </>
  );
}

export function Header({
  conversationCount,
  title,
  model,
  tokens,
  ctx,
  clientCount,
  connected,
  planDone,
  planTotal,
  navOpen,
  railOpen,
  onToggleConversations,
  onToggleFiles,
  onToggleProviders,
}: HeaderProps) {
  const [mobileOpen, setMobileOpen] = useState(false);
  const status = { model, tokens, ctx, clientCount, connected };

  const openProviders = () => {
    setMobileOpen(false);
    onToggleProviders();
  };

  return (
    <>
      <header className="flex shrink-0 items-center gap-2 border-b border-border bg-panel px-3 py-2">
        <Button
          variant="ghost"
          size="sm"
          onClick={onToggleConversations}
          aria-label="Conversations"
          aria-expanded={navOpen}
        >
          <MessagesSquare className="h-4 w-4" />
          {conversationCount > 1 && <span className="text-xs">{conversationCount}</span>}
        </Button>
        <div className="flex min-w-0 flex-1 items-baseline gap-2">
          <span className="shrink-0 font-semibold tracking-tight">aigem</span>
          {title && <span className="truncate text-sm text-muted">{title}</span>}
        </div>

        {/* Progress stays in the bar at every width: with the rail closed on a
            phone, this is the only place the plan is visible at all. */}
        {planTotal ? (
          <Badge
            aria-label={`Plan ${planDone} of ${planTotal} done`}
            className={cn("shrink-0 font-mono", planDone === planTotal && "border-good/40 text-good")}
          >
            <ListChecks className="mr-1 h-3 w-3" />
            {planDone}/{planTotal}
          </Badge>
        ) : null}

        {/* Beside the conversations toggle at every width: behind the overflow
            menu, the plan and the changed files were two taps deep on the screen
            with the least room to spare. */}
        <Button
          variant="ghost"
          size="icon"
          className="shrink-0"
          onClick={onToggleFiles}
          aria-label="Session"
          aria-expanded={railOpen}
        >
          <FileDiff className="h-4 w-4" />
        </Button>

        <div role="group" aria-label="Desktop session controls" className="hidden shrink-0 items-center gap-2 lg:flex">
          <Button variant="ghost" size="icon" onClick={openProviders} aria-label="Providers">
            <KeyRound className="h-4 w-4" />
          </Button>
          <SessionBadges {...status} />
        </div>

        {!connected && <WifiOff className="h-4 w-4 shrink-0 text-warn lg:hidden" aria-label="Reconnecting" />}
        <Button
          variant="ghost"
          size="icon"
          className="shrink-0 lg:hidden"
          onClick={() => setMobileOpen((open) => !open)}
          aria-label="More controls"
          aria-expanded={mobileOpen}
          aria-controls="mobile-header-controls"
        >
          <MoreHorizontal className="h-4 w-4" />
        </Button>
      </header>

      {mobileOpen && (
        <div
          id="mobile-header-controls"
          role="group"
          aria-label="Mobile session controls"
          className="shrink-0 overflow-x-auto border-b border-border bg-panel-2 lg:hidden"
        >
          <div className="flex min-w-max items-center gap-2 px-3 py-2">
            <Button variant="ghost" size="sm" onClick={openProviders}>
              <KeyRound className="h-4 w-4" /> Providers
            </Button>
            <SessionBadges {...status} />
          </div>
        </div>
      )}
    </>
  );
}
