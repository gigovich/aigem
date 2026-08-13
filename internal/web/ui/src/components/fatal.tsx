/** The screen for a failure the reader has to act on, rather than one the page
 *  can retry around: no token, or a daemon that stopped answering before
 *  anything had been drawn.
 *
 *  It is a block, not a toast. An error the reader must act on has to stay on
 *  screen, and it names the one action that resolves this one. */
export function Fatal({ error }: { error: string }) {
  return (
    // Not centred: the block reads from the top-left like everything else here,
    // and a message the reader has to act on should not first have to be found.
    <div className="p-6">
      <div className="max-w-[68ch]">
        <p className="font-mono text-[14px] text-bad">{error}</p>
        <p className="mt-2 text-[13px] text-muted">
          Open the URL the daemon printed - it carries the token.
        </p>
      </div>
    </div>
  );
}
