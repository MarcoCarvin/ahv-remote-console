export type IconName =
  | 'power-dot'
  | 'search'
  | 'chevron-down'
  | 'arrow-right'
  | 'plus'
  | 'close'
  | 'keyboard'
  | 'disc'
  | 'clipboard'
  | 'fit'
  | 'globe'
  | 'fullscreen'
  | 'endpoint-badge'
  | 'refresh-pulse'
  | 'lock'
  | 'unlock'
  | 'info'
  | 'layers'
  | 'power-on'
  | 'power-off'
  | 'power-cycle';

const PATHS: Record<IconName, React.ReactNode> = {
  'power-dot': <circle cx="12" cy="12" r="4" fill="currentColor" />,
  search: (
    <>
      <circle cx="10.5" cy="10.5" r="6.5" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <path
        d="M15.2 15.2 L20 20"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
    </>
  ),
  'chevron-down': (
    <path
      d="M6 9 L12 15 L18 9"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
  'arrow-right': (
    <path
      d="M5 12 L19 12 M13 6 L19 12 L13 18"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
  plus: (
    <path
      d="M12 5 L12 19 M5 12 L19 12"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
    />
  ),
  close: (
    <path
      d="M6 6 L18 18 M18 6 L6 18"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
    />
  ),
  keyboard: (
    <>
      <rect
        x="2.5"
        y="6"
        width="19"
        height="12"
        rx="2"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
      />
      <path
        d="M7 10 L7 10.01 M11 10 L11 10.01 M15 10 L15 10.01"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
      <path
        d="M7 14 L17 14"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
    </>
  ),
  disc: (
    <>
      <circle cx="12" cy="12" r="8.5" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <circle cx="12" cy="12" r="2.5" fill="none" stroke="currentColor" strokeWidth="1.75" />
    </>
  ),
  clipboard: (
    <>
      <path d="M9 4 H15 V7 H9 Z" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <path
        d="M9 5.5 H6 A2 2 0 0 0 4 7.5 V19 A2 2 0 0 0 6 21 H18 A2 2 0 0 0 20 19 V7.5 A2 2 0 0 0 18 5.5 H15"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
      <path
        d="M8 12 L12 8 L16 12 M12 8 L12 18"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </>
  ),
  fit: (
    <>
      <rect
        x="3"
        y="3"
        width="18"
        height="18"
        rx="2"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
      />
      <path
        d="M8 8 L11 11 M8 8 L8 11 M8 8 L11 8 M16 16 L13 13 M16 16 L16 13 M16 16 L13 16"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </>
  ),
  globe: (
    <>
      <circle cx="12" cy="12" r="8.5" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <path d="M3.5 12 L20.5 12" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <path
        d="M12 3.5 C 14.5 6 16 9 16 12 C 16 15 14.5 18 12 20.5 C 9.5 18 8 15 8 12 C 8 9 9.5 6 12 3.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
      />
    </>
  ),
  fullscreen: (
    <path
      d="M4 9 L4 4 L9 4 M20 9 L20 4 L15 4 M4 15 L4 20 L9 20 M20 15 L20 20 L15 20"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
  'endpoint-badge': (
    <>
      <circle cx="12" cy="12" r="4" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <path
        d="M12 4 L12 6 M12 18 L12 20 M4 12 L6 12 M18 12 L20 12 M6.3 6.3 L7.7 7.7 M16.3 16.3 L17.7 17.7 M6.3 17.7 L7.7 16.3 M16.3 7.7 L17.7 6.3"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
    </>
  ),
  'refresh-pulse': (
    <path
      d="M20 12 A8 8 0 1 1 16 5.2 M20 4 L20 9 L15 9"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
  lock: (
    <>
      <rect
        x="5"
        y="11"
        width="14"
        height="9"
        rx="2"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
      />
      <path
        d="M8 11 V8 a4 4 0 0 1 8 0 V11"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
      <circle cx="12" cy="15.5" r="1.25" fill="currentColor" />
    </>
  ),
  unlock: (
    <>
      <rect
        x="5"
        y="11"
        width="14"
        height="9"
        rx="2"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
      />
      {/* Open shackle: hinged on the left, swung clearly up-and-away so the
          right leg is OUT of the body — an unmistakable "unlocked" silhouette
          versus the closed arch of `lock`. */}
      <path
        d="M8 11 V6.5 a4 4 0 0 1 7.6 -1.6"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
      <circle cx="12" cy="15.5" r="1.25" fill="currentColor" />
    </>
  ),
  info: (
    <>
      <circle cx="12" cy="12" r="8.5" fill="none" stroke="currentColor" strokeWidth="1.75" />
      <path
        d="M12 11 L12 16.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
      <circle cx="12" cy="8" r="0.75" fill="currentColor" />
    </>
  ),
  // Stacked layers — point-in-time saved states. Used for VM snapshots
  // (distinct from `disc`/CD, and leaves a future `camera` free for screenshots).
  layers: (
    <>
      <path
        d="M12 3 L20.5 7.5 L12 12 L3.5 7.5 Z"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinejoin="round"
      />
      <path
        d="M3.5 12 L12 16.5 L20.5 12"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <path
        d="M3.5 16.5 L12 21 L20.5 16.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </>
  ),
  // Standard IEC power glyph: a broken ring with a vertical bar through the gap.
  'power-on': (
    <>
      <path
        d="M12 4 L12 11"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
      <path
        d="M7.5 7 A7 7 0 1 0 16.5 7"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
      />
    </>
  ),
  // Shut down: the same power glyph silhouette but filled as a stop square so
  // it reads as the destructive counterpart to power-on.
  'power-off': (
    <rect
      x="6.5"
      y="6.5"
      width="11"
      height="11"
      rx="2"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
    />
  ),
  // Restart / cycle: a near-closed circular arrow.
  'power-cycle': (
    <path
      d="M19 12 A7 7 0 1 1 16.5 6.6 M16.5 3.5 L16.5 7 L13 7"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
};

export function Icon({
  name,
  size = 16,
  className,
  style,
}: {
  name: IconName;
  size?: number;
  className?: string;
  style?: React.CSSProperties;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={className}
      style={style}
      aria-hidden
    >
      {PATHS[name]}
    </svg>
  );
}
