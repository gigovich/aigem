import { cva, type VariantProps } from "class-variance-authority";
import type { ButtonHTMLAttributes, HTMLAttributes, ReactNode, Ref } from "react";
import { cn } from "@/lib/utils";

const button = cva(
  "inline-flex items-center justify-center gap-1.5 rounded-md font-medium " +
    "transition-colors duration-[120ms] ease-out " +
    "disabled:pointer-events-none disabled:opacity-50 " +
    // Tactile push, not a scale and not a glow. Keyboard focus is the only ring.
    "active:translate-y-px " +
    // 44px on a coarse pointer. Keyed off the pointer rather than the viewport,
    // because a touch laptop is a wide screen someone still taps.
    "[@media(pointer:coarse)]:min-h-11 [@media(pointer:coarse)]:min-w-11 " +
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 " +
    "focus-visible:ring-offset-1 focus-visible:ring-offset-canvas select-none",
  {
    variants: {
      variant: {
        default: "bg-accent text-canvas hover:bg-accent/90",
        outline: "border border-line bg-panel text-fg hover:bg-raised",
        ghost: "text-muted hover:bg-raised hover:text-fg",
        // Closing a conversation is not undoable, so it is always labelled and
        // always carries the failure colour rather than a neutral one.
        danger: "border border-bad/40 bg-bad/12 text-bad hover:bg-bad/20",
      },
      size: {
        // 36px minimum: this is used on a phone, where a 28px target is a miss.
        default: "h-9 px-3 text-[13px]",
        sm: "h-8 px-2.5 text-[12px]",
        icon: "h-9 w-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof button> {
  /** Forwarded so a caller can hand focus back to the control a popover was
   *  opened from. Closing one unmounts whatever had focus, and without this a
   *  keyboard reader is dropped at the top of the document. */
  ref?: Ref<HTMLButtonElement>;
}

export function Button({ className, variant, size, ...props }: ButtonProps) {
  return <button className={cn(button({ variant, size }), className)} {...props} />;
}

export function Badge({
  children,
  className,
  ...props
}: HTMLAttributes<HTMLSpanElement> & { children: ReactNode }) {
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-sm border border-line bg-raised " +
          "px-1.5 py-0.5 text-[11px] leading-none font-medium text-muted",
        className,
      )}
      {...props}
    >
      {children}
    </span>
  );
}

/** "This is running", the one live indicator in the interface. A spinner says
 *  the same thing with more ink and a rotation nobody can read a rate from; the
 *  reduced-motion fallback here is the same dot, holding still. */
export function RunDot({ className, label }: { className?: string; label?: string }) {
  return (
    <span
      // A named graphic when it is the only thing saying "running", and hidden
      // when adjacent text already says it. Never a live region: a dozen of
      // these appear in a turn, and none of them is an announcement.
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      className={cn("inline-block h-1.5 w-1.5 shrink-0 rounded-sm bg-accent animate-run", className)}
    />
  );
}

/** A block the exact size of the content that will replace it. The point is
 *  that the layout does not move when the answer lands. */
function Skeleton({ className }: { className?: string }) {
  return (
    <span
      aria-hidden
      className={cn("relative block overflow-hidden rounded-sm bg-raised", className)}
    >
      <span
        className={cn(
          "absolute inset-0 animate-shimmer",
          "bg-gradient-to-r from-transparent via-line-faint to-transparent",
        )}
      />
    </span>
  );
}

/** Rows of skeleton at the height of a list row, for a list that is loading.
 *
 *  rowClass is the height of the row that will replace it. The default is a
 *  list row; a caller whose rows are taller has to say so, because the point of
 *  a skeleton is that the layout does not move when the answer lands. */
export function SkeletonRows({
  rows = 3,
  rowClass = "h-8",
  className,
}: {
  rows?: number;
  rowClass?: string;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col gap-1.5", className)} aria-hidden>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className={cn("w-full", rowClass)} />
      ))}
    </div>
  );
}
