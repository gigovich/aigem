type Props = {
  value: string
  onChange: (value: string) => void
  label: string
  className?: string
}

/** The one input `/` focuses: a screen with a list to search draws one of these. */
export function FilterInput({ value, onChange, label, className = '' }: Props) {
  return (
    <input
      data-filter
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={`${label}…  /`}
      aria-label={label}
      className={`h-6.5 max-w-full rounded-md border border-line bg-bg px-2.5 text-[0.8125rem] outline-none focus:border-primary ${className}`}
    />
  )
}
