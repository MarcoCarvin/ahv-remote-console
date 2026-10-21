import { m } from 'framer-motion';
import { forwardRef, type ReactNode, type ButtonHTMLAttributes } from 'react';

export interface IconButtonProps extends Omit<
  ButtonHTMLAttributes<HTMLButtonElement>,
  | 'onAnimationStart'
  | 'onAnimationEnd'
  | 'onAnimationIteration'
  | 'onDragStart'
  | 'onDragEnd'
  | 'onDrag'
> {
  label: string;
  size?: number;
  children: ReactNode;
}

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { label, size = 32, children, style, disabled, ...rest },
  ref,
) {
  return (
    <m.button
      ref={ref}
      aria-label={label}
      title={label}
      whileTap={disabled ? undefined : { scale: 0.92 }}
      disabled={disabled}
      style={{
        width: size,
        height: size,
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        borderRadius: 'var(--hc-radius-md)',
        color: 'var(--hc-fg-1)',
        background: 'transparent',
        transition: 'background 120ms ease, color 120ms ease',
        ...style,
      }}
      onMouseEnter={(e) => {
        (e.currentTarget as HTMLElement).style.background = 'var(--hc-bg-elev)';
        (e.currentTarget as HTMLElement).style.color = 'var(--hc-fg-0)';
      }}
      onMouseLeave={(e) => {
        (e.currentTarget as HTMLElement).style.background = 'transparent';
        (e.currentTarget as HTMLElement).style.color = 'var(--hc-fg-1)';
      }}
      {...rest}
    >
      {children}
    </m.button>
  );
});
