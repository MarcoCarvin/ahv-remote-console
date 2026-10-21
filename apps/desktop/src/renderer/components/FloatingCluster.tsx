import { useState } from 'react';
import { Icon, type IconName } from './Icon';
import { OverlayMenu } from './console/Overlay';

export interface ClusterMenuItem {
  label: string;
  /** Omitted for `header` rows (non-interactive section labels). */
  onClick?: () => void;
  danger?: boolean;
  /** Non-interactive section heading (groups a long menu, e.g. Send Keys).
   *  Rendered as a muted label; no `onClick`. */
  header?: boolean;
}

export interface ClusterButton {
  icon: IconName;
  title: string;
  onClick?: () => void;
  menu?: ClusterMenuItem[];
  /** Visual emphasis in the rail: primary = filled blue, danger = amber icon. */
  tone?: 'primary' | 'danger';
  /** Fired when a menu button opens (transition closed → open). Lets a menu
   *  load its contents lazily, e.g. the on-demand catalog ISO list. */
  onOpen?: () => void;
}

export function ClusterBtn({ btn }: { btn: ClusterButton }) {
  const [hover, setHover] = useState(false);
  // The menu now renders as a centered overlay (OverlayMenu), which owns its own
  // backdrop / Esc dismissal — so no outside-click listener is needed here.
  const [menuOpen, setMenuOpen] = useState(false);

  const primary = btn.tone === 'primary';
  const danger = btn.tone === 'danger';
  const bg = primary ? 'var(--blue)' : hover || menuOpen ? 'rgba(255,255,255,0.08)' : 'transparent';
  const iconColor = primary ? '#fff' : danger ? '#F2A65A' : 'rgba(255,255,255,0.82)';

  return (
    <div style={{ position: 'relative' }}>
      <button
        onClick={() => {
          if (btn.menu) {
            // Compute the transition, set state purely, then fire onOpen as a
            // SIDE EFFECT in the handler — never inside the setState updater
            // (React runs updaters during render, so a side-effecting onOpen
            // there triggers "setState during render" against ConsoleView).
            const opening = !menuOpen;
            setMenuOpen(opening);
            if (opening) btn.onOpen?.();
          } else {
            btn.onClick?.();
          }
        }}
        onMouseEnter={() => setHover(true)}
        onMouseLeave={() => {
          if (!menuOpen) setHover(false);
        }}
        aria-label={btn.title}
        title={btn.title}
        style={{
          // ≥40px keyboard/touch hit-area via padding while staying 28px visually.
          width: 28,
          height: 28,
          padding: 0,
          background: bg,
          border: 'none',
          color: iconColor,
          display: 'grid',
          placeItems: 'center',
          borderRadius: 9,
          cursor: 'pointer',
          transition: 'background 100ms',
        }}
      >
        <Icon name={btn.icon} size={16} />
      </button>
      {hover && !menuOpen && (
        <div
          style={{
            position: 'absolute',
            top: 'calc(100% + 6px)',
            left: '50%',
            transform: 'translateX(-50%)',
            whiteSpace: 'nowrap',
            background: 'rgba(15, 34, 53, 0.95)',
            color: 'var(--hc-fg-0)',
            fontSize: 11,
            fontFamily: 'var(--mono)',
            padding: '4px 8px',
            borderRadius: 4,
            pointerEvents: 'none',
            boxShadow: '0 2px 8px rgba(0,0,0,0.3)',
          }}
        >
          {btn.title}
        </div>
      )}
      {menuOpen && btn.menu && (
        <OverlayMenu
          title={btn.title}
          items={btn.menu}
          onClose={() => {
            setMenuOpen(false);
            setHover(false);
          }}
        />
      )}
    </div>
  );
}
