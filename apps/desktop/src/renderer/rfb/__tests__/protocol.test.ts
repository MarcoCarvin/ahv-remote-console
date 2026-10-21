/**
 * RFB protocol constants — smoke tests.
 *
 * The constants in protocol.ts pin the wire format. A typo here breaks
 * the entire client. These tests are intentionally redundant with the
 * source (we duplicate the literal values) so a one-line edit to either
 * the spec or the source surfaces as a test failure during review.
 */
import { describe, expect, it } from 'vitest';
import {
  CLI_MSG_SET_PIXEL_FORMAT,
  CLI_MSG_SET_ENCODINGS,
  CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST,
  CLI_MSG_KEY_EVENT,
  CLI_MSG_POINTER_EVENT,
  CLI_MSG_CLIENT_CUT_TEXT,
  CLI_MSG_SET_DESKTOP_SIZE,
  CLI_MSG_ENABLE_CONTINUOUS_UPDATES,
  CLI_MSG_QEMU,
  QEMU_SUBMSG_EXT_KEY,
  SRV_MSG_FRAMEBUFFER_UPDATE,
  SRV_MSG_SET_COLOUR_MAP_ENTRIES,
  SRV_MSG_BELL,
  SRV_MSG_SERVER_CUT_TEXT,
  SRV_MSG_END_OF_CONTINUOUS_UPDATES,
  ENC_RAW,
  ENC_COPY_RECT,
  ENC_RRE,
  ENC_HEXTILE,
  ENC_ZRLE,
  ENC_CURSOR,
  ENC_DESKTOP_SIZE,
  ENC_EXTENDED_DESKTOP_SIZE,
  ENC_CONTINUOUS_UPDATES,
  ENC_QEMU_EXT_KEY_EVENT,
  SEC_NONE,
  SEC_VNC_AUTH,
  SEC_TIGHT,
  BTN_LEFT,
  BTN_MIDDLE,
  BTN_RIGHT,
  BTN_WHEEL_UP,
  BTN_WHEEL_DOWN,
} from '../protocol';

describe('RFB protocol constants', () => {
  describe('client→server message types (RFC 6143 §7.5)', () => {
    it('matches the RFC numeric assignments', () => {
      expect(CLI_MSG_SET_PIXEL_FORMAT).toBe(0);
      expect(CLI_MSG_SET_ENCODINGS).toBe(2);
      expect(CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST).toBe(3);
      expect(CLI_MSG_KEY_EVENT).toBe(4);
      expect(CLI_MSG_POINTER_EVENT).toBe(5);
      expect(CLI_MSG_CLIENT_CUT_TEXT).toBe(6);
    });

    it('matches the extension numeric assignments', () => {
      expect(CLI_MSG_SET_DESKTOP_SIZE).toBe(251);
      expect(CLI_MSG_ENABLE_CONTINUOUS_UPDATES).toBe(150);
      expect(CLI_MSG_QEMU).toBe(255);
      expect(QEMU_SUBMSG_EXT_KEY).toBe(0);
    });
  });

  describe('server→client message types (RFC 6143 §7.6)', () => {
    it('matches the RFC numeric assignments', () => {
      expect(SRV_MSG_FRAMEBUFFER_UPDATE).toBe(0);
      expect(SRV_MSG_SET_COLOUR_MAP_ENTRIES).toBe(1);
      expect(SRV_MSG_BELL).toBe(2);
      expect(SRV_MSG_SERVER_CUT_TEXT).toBe(3);
      expect(SRV_MSG_END_OF_CONTINUOUS_UPDATES).toBe(150);
    });
  });

  describe('encoding type identifiers', () => {
    it('uses positive numbers for standard encodings (RFC 6143 §7.7)', () => {
      expect(ENC_RAW).toBe(0);
      expect(ENC_COPY_RECT).toBe(1);
      expect(ENC_RRE).toBe(2);
      expect(ENC_HEXTILE).toBe(5);
      expect(ENC_ZRLE).toBe(16);
    });

    it('uses negative numbers for pseudo-encodings (RFC 6143 §7.8)', () => {
      expect(ENC_CURSOR).toBe(-239);
      expect(ENC_DESKTOP_SIZE).toBe(-223);
      expect(ENC_EXTENDED_DESKTOP_SIZE).toBe(-308);
      expect(ENC_CONTINUOUS_UPDATES).toBe(-313);
      expect(ENC_QEMU_EXT_KEY_EVENT).toBe(-258);
    });
  });

  describe('security types', () => {
    it('matches RFC 6143 §7.2 assignments', () => {
      expect(SEC_NONE).toBe(1);
      expect(SEC_VNC_AUTH).toBe(2);
      expect(SEC_TIGHT).toBe(16);
    });
  });

  describe('pointer button bitmasks', () => {
    it('uses single-bit flags so they OR together cleanly', () => {
      expect(BTN_LEFT).toBe(0x01);
      expect(BTN_MIDDLE).toBe(0x02);
      expect(BTN_RIGHT).toBe(0x04);
      expect(BTN_WHEEL_UP).toBe(0x08);
      expect(BTN_WHEEL_DOWN).toBe(0x10);
      // No overlap: each bit is unique.
      const all = BTN_LEFT | BTN_MIDDLE | BTN_RIGHT | BTN_WHEEL_UP | BTN_WHEEL_DOWN;
      expect(all).toBe(0x1f);
    });
  });

  describe('cross-checks', () => {
    it('client and server "continuous updates" messages share the 150 number', () => {
      // RFC: client sends type 150 to enable, server replies with type 150 to end.
      expect(CLI_MSG_ENABLE_CONTINUOUS_UPDATES).toBe(SRV_MSG_END_OF_CONTINUOUS_UPDATES);
    });

    it('all encoding values fit in a signed 32-bit integer (the RFB wire field width)', () => {
      const all = [
        ENC_RAW,
        ENC_COPY_RECT,
        ENC_RRE,
        ENC_HEXTILE,
        ENC_ZRLE,
        ENC_CURSOR,
        ENC_DESKTOP_SIZE,
        ENC_EXTENDED_DESKTOP_SIZE,
        ENC_CONTINUOUS_UPDATES,
        ENC_QEMU_EXT_KEY_EVENT,
      ];
      for (const v of all) {
        expect(v).toBeGreaterThanOrEqual(-0x80000000);
        expect(v).toBeLessThanOrEqual(0x7fffffff);
      }
    });
  });
});
