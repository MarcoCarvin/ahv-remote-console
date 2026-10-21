import { m } from 'framer-motion';
import { forwardRef, type ButtonHTMLAttributes, type CSSProperties, type ReactNode } from 'react';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';
export type ButtonSize = 'sm' | 'md' | 'lg';

// framer-motion 12 reshaped MotionStyle into a union with motion-value
// variants for every CSS property. For a primitive that does not animate
// motion-values, we type props as ButtonHTMLAttributes and cast at the
// motion boundary.
export interface ButtonProps extends Omit<
  ButtonHTMLAttributes<HTMLButtonElement>,
  | 'onAnimationStart'
  | 'onAnimationEnd'
  | 'onAnimationIteration'
  | 'onDragStart'
  | 'onDragEnd'
  | 'onDrag'
> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  iconLeft?: ReactNode;
  iconRight?: ReactNode;
  children?: ReactNode;
}

const VARIANT_STYLE: Record<ButtonVariant, CSSProperties> = {
  primary: {
    background: 'var(--hc-accent-500)',
    color: '#fff',
    boxShadow: '0 4px 18px -6px rgba(43, 109, 240, 0.55)',
  },
  secondary: {
    background: 'var(--hc-bg-elev)',
    color: 'var(--hc-fg-0)',
    border: '1px solid var(--hc-border-1)',
  },
  ghost: {
    background: 'transparent',
    color: 'var(--hc-fg-1)',
  },
  danger: {
    background: 'var(--hc-error)',
    color: '#fff',
  },
};

const SIZE_STYLE: Record<ButtonSize, CSSProperties> = {
  sm: { padding: '6px 10px', fontSize: 12, borderRadius: 'var(--hc-radius-sm)' },
  md: { padding: '9px 14px', fontSize: 13, borderRadius: 'var(--hc-radius-md)' },
  lg: { padding: '12px 18px', fontSize: 14, borderRadius: 'var(--hc-radius-md)' },
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  {
    variant = 'primary',
    size = 'md',
    loading,
    iconLeft,
    iconRight,
    children,
    disabled,
    style,
    ...rest
  },
  ref,
) {
  const composed: CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 8,
    fontWeight: 500,
    cursor: disabled || loading ? 'not-allowed' : 'pointer',
    opacity: disabled || loading ? 0.6 : 1,
    transition:
      'transform 120ms var(--hc-easing-emphasized), background 160ms ease, opacity 160ms ease',
    ...VARIANT_STYLE[variant],
    ...SIZE_STYLE[size],
    ...style,
  };

  return (
    <m.button
      ref={ref}
      whileTap={disabled || loading ? undefined : { scale: 0.97 }}
      whileHover={disabled || loading ? undefined : { y: -1 }}
      disabled={disabled ?? loading}
      style={composed}
      {...rest}
    >
      {loading ? <Spinner size={14} /> : iconLeft}
      {children}
      {iconRight}
    </m.button>
  );
});

function Spinner({ size = 14 }: { size?: number }) {
  return (
    <m.span
      aria-hidden
      style={{
        display: 'inline-block',
        width: size,
        height: size,
        borderRadius: '50%',
        border: '2px solid rgba(255,255,255,0.3)',
        borderTopColor: '#fff',
      }}
      animate={{ rotate: 360 }}
      transition={{ duration: 0.9, repeat: Infinity, ease: 'linear' }}
    />
  );
}
