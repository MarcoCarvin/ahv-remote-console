import { afterEach, describe, expect, it, vi } from 'vitest';
import { detectHostKeyboardLayout } from '../HostOS';

// detectHostKeyboardLayout reads navigator.language / navigator.languages. We
// stub the global navigator per-case so the default-Italian contract is
// deterministic regardless of the host running the suite. detectHostKeyboardLayout
// is the un-cached probe (hostKeyboardLayout() memoizes the first call), so we
// test the probe directly.
function withLanguage(language: string | undefined, run: () => void) {
  const original = globalThis.navigator;
  const nav =
    language === undefined
      ? ({} as unknown as Navigator)
      : ({ language, languages: [language] } as unknown as Navigator);
  vi.stubGlobal('navigator', nav);
  try {
    run();
  } finally {
    vi.stubGlobal('navigator', original);
  }
}

describe('detectHostKeyboardLayout — Italian recovery is locale-scoped', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('falls back to US passthrough when the locale is undetectable', () => {
    withLanguage(undefined, () => {
      expect(detectHostKeyboardLayout()).toBe('us');
    });
  });

  it('returns Italian only for an explicit Italian locale', () => {
    withLanguage('it-IT', () => expect(detectHostKeyboardLayout()).toBe('it'));
    withLanguage('it', () => expect(detectHostKeyboardLayout()).toBe('it'));
  });

  it('returns US passthrough for an explicit English locale', () => {
    withLanguage('en-US', () => expect(detectHostKeyboardLayout()).toBe('us'));
    withLanguage('en-GB', () => expect(detectHostKeyboardLayout()).toBe('us'));
    withLanguage('en', () => expect(detectHostKeyboardLayout()).toBe('us'));
  });

  it('returns US passthrough for other non-English, non-Italian locales (no longer mangles their symbol keys)', () => {
    // Regression: these previously fell through to Italian recovery, which
    // rewrote their layout-specific non-ASCII symbol-key glyphs (e.g. German
    // 'ä' on Quote) to the wrong en-us symbol. They now pass through unchanged.
    withLanguage('de-DE', () => expect(detectHostKeyboardLayout()).toBe('us'));
    withLanguage('fr-FR', () => expect(detectHostKeyboardLayout()).toBe('us'));
    withLanguage('es-ES', () => expect(detectHostKeyboardLayout()).toBe('us'));
  });
});
