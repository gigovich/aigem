import { useState, type ReactNode } from "react";
import { FileDiff, KeyRound, ListChecks, MessagesSquare, MoreHorizontal, WifiOff } from "lucide-react";
import { Badge, Button } from "./ui";
import { cn } from "@/lib/utils";

interface HeaderProps {
  /** The top-level screen selector, when this daemon serves more than one. */
  modeSwitch?: ReactNode;
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
      {model && <Badge className="max-w-40 truncate font-mono">{model}</Badge>}
      {/* Every number in this interface is monospaced, so a context figure that
          grows a digit mid-turn does not shift the badges beside it. */}
      {tokens > 0 && (
        <Badge className="font-mono">
          {Math.round(tokens / 1000)}k
          {ctx > 0 && ` / ${Math.round(ctx / 1000)}k`} ctx
        </Badge>
      )}
      {clientCount > 1 && <Badge className="font-mono">{clientCount} attached</Badge>}
      {/* Not the accent: a socket retrying itself is not a decision the reader
          has to make, and this badge sits beside ones that are. */}
      {!connected && (
        <Badge>
          <WifiOff className="mr-1 h-3 w-3" aria-hidden /> reconnecting
        </Badge>
      )}
    </>
  );
}

export function Header({
  modeSwitch,
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
      <header className="flex h-11 shrink-0 items-center gap-2 border-b border-line bg-panel px-3">
        <Button
          variant="ghost"
          size="sm"
          onClick={onToggleConversations}
          aria-label="Conversations"
          aria-expanded={navOpen}
        >
          <MessagesSquare className="h-4 w-4" />
          {conversationCount > 1 && (
            <span className="font-mono text-[12px]">{conversationCount}</span>
          )}
        </Button>
        <div className="flex min-w-0 flex-1 items-center gap-2">
          <span className="shrink-0 text-[15px] font-medium">aigem</span>
          {modeSwitch}
          {title && <span className="truncate text-[13px] text-muted">{title}</span>}
        </div>

        {/* Progress stays in the bar at every width: with the rail closed on a
            phone, this is the only place the plan is visible at all. */}
        {planTotal ? (
          <Badge
            aria-label={`Plan ${planDone} of ${planTotal} done`}
            className={cn("shrink-0 font-mono", planDone === planTotal && "border-good/40 text-good")}
          >
            <ListChecks className="mr-1 h-3 w-3" aria-hidden />
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

        <div role="group" aria-label="Desktop session controls" className="hidden shrink-0 items-center gap-2 md:flex">
          <Button variant="ghost" size="icon" onClick={openProviders} aria-label="Providers">
            <KeyRound className="h-4 w-4" />
          </Button>
          <SessionBadges {...status} />
        </div>

        {!connected && (
          <WifiOff className="h-4 w-4 shrink-0 text-muted md:hidden" role="img" aria-label="Reconnecting" />
        )}
        <Button
          variant="ghost"
          size="icon"
          className="shrink-0 md:hidden"
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
          className="shrink-0 overflow-x-auto border-b border-line bg-raised md:hidden"
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
