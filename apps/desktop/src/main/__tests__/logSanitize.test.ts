import { describe, expect, it } from 'vitest';
import { sanitizeLogField, MAX_LOG_FIELD } from '../logSanitize';

/**
 * RED-04 / ELEC-06: hc-log writes renderer-controlled `[tag] message` to the
 * shared log file. These tests prove the sanitizer strips the CR/LF that would
 * let a renderer forge log records, drops other control chars, and caps length.
 */
describe('sanitizeLogField', () => {
  it('strips CR and LF (the log-forging vector)', () => {
    const forged = 'real line\n2026-01-01 12:00:00 [admin] FORGED ADMIN ACTION';
    const out = sanitizeLogField(forged);
    expect(out).not.toContain('\n');
    expect(out).not.toContain('\r');
    // One contiguous line — no embedded newline anywhere.
    expect(out.split('\n')).toHaveLength(1);
  });

  it('strips a CRLF pair', () => {
    expect(sanitizeLogField('a\r\nb')).toBe('a  b');
  });

  it('strips TAB and other C0 control chars', () => {
    expect(sanitizeLogField('a\tb')).toBe('a b');
    expect(sanitizeLogField('a\x00b\x1fc')).toBe('a b c');
  });

  it('strips DEL (0x7f) and C1 control chars (0x80-0x9f)', () => {
    expect(sanitizeLogField('a\x7fb')).toBe('a b');
    expect(sanitizeLogField('a\x85b')).toBe('a b'); // NEL, a C1 line break
    expect(sanitizeLogField('a\x9fb')).toBe('a b');
  });

  it('preserves ordinary printable text including unicode above C1', () => {
    expect(sanitizeLogField('clip: ack timeout (seq 42) — café ✓')).toBe(
      'clip: ack timeout (seq 42) — café ✓',
    );
  });

  it('caps length at MAX_LOG_FIELD', () => {
    const huge = 'x'.repeat(MAX_LOG_FIELD + 5000);
    expect(sanitizeLogField(huge)).toHaveLength(MAX_LOG_FIELD);
  });

  it('honors a custom cap', () => {
    expect(sanitizeLogField('abcdef', 3)).toBe('abc');
  });

  it('leaves a short clean string untouched', () => {
    expect(sanitizeLogField('reconciler.tx state=IDLE->ARMED')).toBe(
      'reconciler.tx state=IDLE->ARMED',
    );
  });
});
