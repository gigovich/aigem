import { cva, type VariantProps } from "class-variance-authority";
import type { ButtonHTMLAttributes, ReactNode } from "react";
import { cn } from "@/lib/utils";

const button = cva(
  "inline-flex items-center justify-center gap-1.5 rounded-md text-sm font-medium " +
    "transition-colors disabled:pointer-events-none disabled:opacity-50 " +
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 select-none",
  {
    variants: {
      variant: {
        default: "bg-accent text-bg hover:bg-accent/90",
        outline: "border border-border bg-panel hover:bg-panel-2 text-fg",
        ghost: "hover:bg-panel-2 text-muted hover:text-fg",
        danger: "bg-bad/15 text-bad border border-bad/40 hover:bg-bad/25",
      },
      size: {
        // 36px minimum: this is used on a phone, where a 28px target is a miss.
        default: "h-9 px-3",
        sm: "h-8 px-2.5 text-xs",
        icon: "h-9 w-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof button> {}

export function Button({ className, variant, size, ...props }: ButtonProps) {
  return <button className={cn(button({ variant, size }), className)} {...props} />;
}

export function Badge({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full border border-border bg-panel-2 " +
          "px-2 py-0.5 text-[11px] font-medium text-muted",
        className,
      )}
    >
      {children}
    </span>
  );
}

export function Spinner({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "inline-block h-3 w-3 animate-spin rounded-full border-2 border-muted border-t-transparent",
        className,
      )}
    />
  );
}
