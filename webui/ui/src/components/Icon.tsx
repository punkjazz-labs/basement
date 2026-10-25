import type { ReactNode } from 'react'

// The console's one icon set: hand-drawn on a 24px grid with a 1.5px round
// stroke, in the manner of Lucide, so every glyph on every screen has the same
// weight. An icon is decoration beside a word that already says the thing, so
// it is hidden from the accessibility tree; a control drawn with an icon alone
// carries its own aria-label.

const PATHS = {
  // The rail.
  models: (
    <>
      <path d="M12 2.8 20.2 7.4v9.2L12 21.2l-8.2-4.6V7.4Z" />
      <path d="m3.8 7.4 8.2 4.6 8.2-4.6" />
      <path d="M12 12v9.2" />
    </>
  ),
  chat: <path d="M20 15.5a2 2 0 0 1-2 2H8l-4 3.5V5.5a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2Z" />,
  generate: (
    <>
      <rect x="3.5" y="3.5" width="17" height="17" rx="2.5" />
      <circle cx="9" cy="9" r="1.75" />
      <path d="m20.5 15-4.3-4.3a1.5 1.5 0 0 0-2.1 0L5 19.8" />
    </>
  ),
  redactor: (
    <>
      <path d="M14 3.5H7a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-10Z" />
      <path d="M14 3.5v5h5" />
      <rect x="8.5" y="12.5" width="7" height="3" rx="0.75" />
    </>
  ),
  minutes: (
    <>
      <rect x="9" y="3" width="6" height="11" rx="3" />
      <path d="M18.5 11a6.5 6.5 0 0 1-13 0" />
      <path d="M12 17.5V21" />
    </>
  ),
  roles: (
    <>
      <path d="M12 3v3" />
      <path d="M12 13v8" />
      <path d="M5 6h11.6a1.5 1.5 0 0 1 1 .4l2.7 2.6-2.7 2.6a1.5 1.5 0 0 1-1 .4H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1Z" />
    </>
  ),
  sparks: (
    <>
      <rect x="3" y="3.5" width="18" height="7" rx="2" />
      <rect x="3" y="13.5" width="18" height="7" rx="2" />
      <path d="M7 7h.01M7 17h.01" />
    </>
  ),
  monitor: <path d="M21 12h-3.5l-2.6 7.5L9.1 4.5 6.5 12H3" />,
  storage: (
    <>
      <path d="M21 12.5H3" />
      <path d="M5.6 5.2 3 12.5V18a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5.5l-2.6-7.3A2 2 0 0 0 16.5 4h-9a2 2 0 0 0-1.9 1.2Z" />
      <path d="M7 16.3h.01M10.5 16.3h.01" />
    </>
  ),
  activity: (
    <>
      <path d="M3.5 12a8.5 8.5 0 1 0 2.6-6.1L3.5 8.4" />
      <path d="M3.5 3.8v4.6h4.6" />
      <path d="M12 7.5V12l3 1.8" />
    </>
  ),
  api: (
    <>
      <path d="m16 17.5 5.5-5.5L16 6.5" />
      <path d="M8 6.5 2.5 12 8 17.5" />
    </>
  ),
  // Everything else.
  chevronRight: <path d="m9.5 6 6 6-6 6" />,
  close: <path d="M18 6 6 18M6 6l12 12" />,
  alert: (
    <>
      <path d="M10.3 3.9 2.6 17.5a2 2 0 0 0 1.7 3h15.4a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z" />
      <path d="M12 9.5v4M12 17h.01" />
    </>
  ),
  failure: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7.8v4.7M12 16.2h.01" />
    </>
  ),
  check: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="m8.5 12.2 2.4 2.4 4.6-4.8" />
    </>
  ),
  external: <path d="M8 16 16 8M9.5 8H16v6.5" />,
  pulse: <path d="M3 12h4l2-4.5 4 9 2-4.5h6" />,
} satisfies Record<string, ReactNode>

export type IconName = keyof typeof PATHS

export function Icon({ name, size = 16, className }: { name: IconName; size?: number; className?: string }) {
  return (
    <svg
      className={className ? `icon ${className}` : 'icon'}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {PATHS[name]}
    </svg>
  )
}
