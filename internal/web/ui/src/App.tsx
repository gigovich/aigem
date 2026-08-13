import { useEffect, useState } from "react";
import { api } from "@/lib/protocol";
import { replaceMode, useMode, type Mode } from "@/lib/route";
import { Fatal } from "@/components/fatal";
import { ModeSwitch } from "@/components/modeswitch";
import { Workspace } from "@/workspace/Workspace";
import { ChatApp } from "@/chat/ChatApp";

interface Modes {
  sessions: boolean;
  chat: boolean;
}

/** One bundle serves both halves of the product, and only the daemon knows
 *  which it is: `aigem web run` has no fleet and `aigem bot start` creates no
 *  sessions. Asking is the only way the page can find out, since both are
 *  served from this origin by the same binary. */
export default function App() {
  const [modes, setModes] = useState<Modes | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [mode, setMode] = useMode();

  useEffect(() => {
    api<Modes>("/api/modes")
      .then(setModes)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  const both = !!modes?.sessions && !!modes?.chat;
  // A link into the half this daemon does not serve is corrected rather than
  // rendered: every request the missing screen makes would 404, which reads as
  // a broken product rather than as the wrong address.
  const active: Mode = both ? mode : modes?.chat ? "chat" : "sessions";
  // In an effect, not in the render body: rendering must not touch history.
  useEffect(() => {
    if (modes && !both) replaceMode(active);
  }, [modes, both, active]);

  // A daemon serving neither is a daemon that would not have started, so this
  // is only ever the moment before the answer lands. Nothing is drawn for it:
  // a skeleton of a screen that may turn out to be the other one is a flicker
  // between two layouts rather than a placeholder for one.
  if (error) return <Fatal error={error} />;
  if (!modes) return null;

  const modeSwitch = both ? <ModeSwitch mode={active} onChange={setMode} /> : undefined;
  return active === "chat" ? (
    <ChatApp modeSwitch={modeSwitch} />
  ) : (
    <Workspace modeSwitch={modeSwitch} />
  );
}
