import { useRef, type CSSProperties, type ReactNode } from 'react';
import { useFocusTrap } from '../hooks/useFocusTrap';

export interface DialogProps {
  open: boolean;
  /** Called on Escape or backdrop click. */
  onClose: () => void;
  children: ReactNode;
  /** Accessible name (one of these should be set for screen readers). */
  'aria-label'?: string;
  'aria-labelledby'?: string;
  className?: string;
  style?: CSSProperties;
}

/**
 * Modal dialog primitive: role="dialog" + aria-modal="true", with focus
 * move-in / trap / restore-on-close (useFocusTrap) and Escape-to-close. The
 * caller supplies the visual surface as children; this only owns the a11y
 * semantics and focus behaviour. Renders nothing while closed.
 */
export function Dialog({
  open,
  onClose,
  children,
  'aria-label': ariaLabel,
  'aria-labelledby': ariaLabelledby,
  className,
  style,
}: DialogProps) {
  const ref = useRef<HTMLDivElement>(null);
  useFocusTrap(ref, open);

  if (!open) return null;

  return (
    <div
      ref={ref}
      role="dialog"
      aria-modal="true"
      aria-label={ariaLabel}
      aria-labelledby={ariaLabelledby}
      className={className}
      style={style}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.stopPropagation();
          onClose();
        }
      }}
    >
      {children}
    </div>
  );
}
