import { forwardRef, type InputHTMLAttributes, type ReactNode } from 'react';

export interface FieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  iconLeft?: ReactNode;
  iconRight?: ReactNode;
}

export const Field = forwardRef<HTMLInputElement, FieldProps>(function Field(
  { label, hint, error, iconLeft, iconRight, style, id, ...rest },
  ref,
) {
  const inputId = id ?? rest.name;
  return (
    <label htmlFor={inputId} style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
      {label ? (
        <span style={{ fontSize: 12, color: 'var(--hc-fg-2)', fontWeight: 500 }}>{label}</span>
      ) : null}
      <span
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 8,
          padding: '8px 12px',
          background: 'var(--hc-bg-2)',
          border: `1px solid ${error ? 'var(--hc-error)' : 'var(--hc-border-0)'}`,
          borderRadius: 'var(--hc-radius-md)',
          transition: 'border-color 120ms ease, box-shadow 120ms ease',
        }}
      >
        {iconLeft ? <span style={{ color: 'var(--hc-fg-2)' }}>{iconLeft}</span> : null}
        <input
          ref={ref}
          id={inputId}
          {...rest}
          style={{
            flex: 1,
            background: 'transparent',
            border: 0,
            outline: 'none',
            color: 'var(--hc-fg-0)',
            ...style,
          }}
        />
        {iconRight ? <span style={{ color: 'var(--hc-fg-2)' }}>{iconRight}</span> : null}
      </span>
      {hint && !error ? (
        <span style={{ fontSize: 11, color: 'var(--hc-fg-2)' }}>{hint}</span>
      ) : null}
      {error ? <span style={{ fontSize: 11, color: 'var(--hc-error)' }}>{error}</span> : null}
    </label>
  );
});
