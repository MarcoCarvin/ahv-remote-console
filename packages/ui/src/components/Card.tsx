import { type HTMLAttributes, forwardRef } from 'react';

export interface CardProps extends HTMLAttributes<HTMLDivElement> {
  elevated?: boolean;
  glass?: boolean;
}

export const Card = forwardRef<HTMLDivElement, CardProps>(function Card(
  { elevated, glass, style, children, ...rest },
  ref,
) {
  return (
    <div
      ref={ref}
      style={{
        background: glass ? 'var(--hc-bg-glass)' : 'var(--hc-bg-1)',
        border: '1px solid var(--hc-border-0)',
        borderRadius: 'var(--hc-radius-lg)',
        boxShadow: elevated ? 'var(--hc-shadow-deep)' : 'var(--hc-shadow-soft)',
        backdropFilter: glass ? 'blur(20px) saturate(140%)' : undefined,
        WebkitBackdropFilter: glass ? 'blur(20px) saturate(140%)' : undefined,
        ...style,
      }}
      {...rest}
    >
      {children}
    </div>
  );
});
