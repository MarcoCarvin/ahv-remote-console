#!/usr/bin/env node
/**
 * build-icons.mjs — generate all app icon assets from the brand spark.
 *
 * Design: a flat, two-tone MONITOR (display + slim stand) on a solid brand
 * ground, with the brand spark KNOCKED OUT of the screen (the ground shows
 * through). No gradients, no glass — exactly two colours per icon.
 *
 *   Source of truth: assets/brand/spark-white.png  (white spark on transparent;
 *   regenerate with scripts/extract-spark.mjs) + the geometry/colours below.
 *
 * Platform shape (per the design brief):
 *   - Windows / Linux / web  → FULL-BLEED square master
 *   - macOS .icns            → SQUIRCLE-padded (art ~82% inside a superellipse)
 *
 * Usage:  node scripts/build-icons.mjs
 * Output: assets/icon/ (PNG sizes, modes/, icon.icns, icon.ico, source SVG)
 *         build/icons/ (icns, ico, tray, NxN, ahv-remote-console-512.png) for
 *         electron-builder + the running app (main.ts ICON_DIR).
 */

import { readFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import sharp from 'sharp';
import * as png2icons from 'png2icons';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = join(__dirname, '..');
const ASSETS = join(ROOT, 'assets', 'icon');
const BUILD = join(ROOT, 'build', 'icons');
const SOURCE = join(ASSETS, 'source');
const MODES = join(ASSETS, 'modes');
const PREVIEW = join(ASSETS, 'preview');
const SPARK = join(ROOT, 'assets', 'brand', 'spark-white.png');

const BLUE = '#157DC1';
const INK = '#15263A';
const WHITE = '#FFFFFF';

const SIZES = [16, 32, 48, 64, 128, 256, 512, 1024];

// macOS superellipse ("squircle") on a 1024 viewBox — used as a clip via viewBox scaling.
const SQUIRCLE_PATH =
  'M 229 0 L 795 0 C 922 0 1024 102 1024 229 L 1024 795 C 1024 922 922 1024 795 1024 ' +
  'L 229 1024 C 102 1024 0 922 0 795 L 0 229 C 0 102 102 0 229 0 Z';

const hexToRgb = (h) => ({
  r: parseInt(h.slice(1, 3), 16),
  g: parseInt(h.slice(3, 5), 16),
  b: parseInt(h.slice(5, 7), 16),
});

const sparkPng = readFileSync(SPARK);

/**
 * The brand spark filled with a SOLID colour at `size`. We mask a solid colour
 * tile by the spark's alpha (dest-in) — NOT sharp.tint(), which preserves
 * luminance and would leave a white spark white.
 */
async function coloredSpark(size, hex) {
  const { r, g, b } = hexToRgb(hex);
  const alpha = await sharp(sparkPng).resize(size, size, { fit: 'contain' }).png().toBuffer();
  return sharp({ create: { width: size, height: size, channels: 4, background: { r, g, b, alpha: 1 } } })
    .composite([{ input: alpha, blend: 'dest-in' }])
    .png()
    .toBuffer();
}

/** Lighten (f>0, toward white) or darken (f<0) a hex colour. */
function adjust(hex, f) {
  const { r, g, b } = hexToRgb(hex);
  const a = (c) => Math.max(0, Math.min(255, Math.round(f >= 0 ? c + (255 - c) * f : c * (1 + f))));
  const h = (c) => a(c).toString(16).padStart(2, '0');
  return `#${h(r)}${h(g)}${h(b)}`;
}

/**
 * Monitor geometry (fractions of the art box A), offset by `pad`. At small
 * sizes (`compact`) we drop the stand and enlarge the screen + spark so the
 * mark survives at taskbar/favicon resolution (the fine spark otherwise muddies
 * at 16px).
 */
function geometry(S, pad, compact = false) {
  const A = S - pad * 2;
  const g = (f) => Math.round(A * f);
  if (compact) {
    const sw = g(0.86),
      sh = g(0.66);
    const sx = pad + Math.round((A - sw) / 2),
      sy = pad + Math.round((A - sh) / 2),
      sr = g(0.08);
    return { sw, sh, sx, sy, sr, nw: 0, nh: 0, nx: 0, ny: 0, bw: 0, bh: 0, bx: 0, by: 0, br: 0, sparkF: 0.98 };
  }
  // Screen width as a fraction of the art box. Centred-object icons conventionally
  // fill ~0.60–0.72 of the safe area (cf. Android product-icon keyshapes ~0.52–0.58
  // + iOS/macOS keyline safe areas); 0.70 reads full without crowding the edges.
  // All monitor parts derive from the screen width, and the whole monitor
  // (screen + stand) is vertically centred in the art box.
  const SCREEN_W = 0.78;
  const sw = g(SCREEN_W);
  const sh = Math.round(sw * 0.7333),
    sr = Math.round(sw * 0.1167);
  const nw = Math.round(sw * 0.125),
    nh = Math.round(sw * 0.0833);
  const bw = Math.round(sw * 0.4167),
    bh = Math.max(2, Math.round(sw * 0.0533)),
    br = Math.round(bh / 2);
  const monH = sh + nh + bh;
  const sy = pad + Math.round((A - monH) / 2);
  const sx = pad + Math.round((A - sw) / 2);
  const ny = sy + sh,
    nx = pad + Math.round((A - nw) / 2);
  const by = ny + nh,
    bx = pad + Math.round((A - bw) / 2);
  return { sw, sh, sx, sy, sr, nw, nh, nx, ny, bw, bh, bx, by, br, sparkF: 0.92 };
}

function monitorSvg(S, geo, mark) {
  const stand =
    geo.nw > 0
      ? `<rect x="${geo.nx}" y="${geo.ny}" width="${geo.nw}" height="${geo.nh}" fill="${mark}"/>
         <rect x="${geo.bx}" y="${geo.by}" width="${geo.bw}" height="${geo.bh}" rx="${geo.br}" fill="${mark}"/>`
      : '';
  return `<svg width="${S}" height="${S}" xmlns="http://www.w3.org/2000/svg">
    <rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" rx="${geo.sr}" fill="${mark}"/>
    ${stand}
  </svg>`;
}

/**
 * Compose one square icon at size S. `squircle` insets the art ~9% and clips the
 * ground to the macOS superellipse. Premium depth: a subtle vertical ground
 * gradient, a soft drop shadow under the monitor (large sizes only), a faint
 * screen sheen, and the spark as a REAL knockout (the gradient shows through).
 */
async function compose(S, ground, mark, { squircle = false } = {}) {
  const pad = squircle ? Math.round(S * 0.09) : 0;
  const geo = geometry(S, pad, S <= 32);
  // Subtle sheen / vignette / specular only where they actually read; small
  // icons (favicon/taskbar) stay crisp and flat.
  const depth = S >= 128;

  // Ground: a DIAGONAL gradient (light top-left → deep bottom-right). At larger
  // sizes a soft top-left sheen lifts it and a faint edge vignette grounds it,
  // so the surface reads dimensional rather than a flat fill.
  const grad = `<linearGradient id="bg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="${adjust(ground, 0.24)}"/><stop offset="0.5" stop-color="${adjust(ground, 0.02)}"/><stop offset="1" stop-color="${adjust(ground, -0.26)}"/></linearGradient>`;
  const sheenBg = `<radialGradient id="sheenBg" cx="0.27" cy="0.2" r="0.9"><stop offset="0" stop-color="#ffffff" stop-opacity="0.2"/><stop offset="0.55" stop-color="#ffffff" stop-opacity="0"/></radialGradient>`;
  const vig = `<radialGradient id="vig" cx="0.5" cy="0.55" r="0.72"><stop offset="0.6" stop-color="#03203a" stop-opacity="0"/><stop offset="1" stop-color="#03203a" stop-opacity="0.24"/></radialGradient>`;
  const groundShape = (fill) =>
    squircle ? `<path d="${SQUIRCLE_PATH}" fill="${fill}"/>` : `<rect width="${S}" height="${S}" fill="${fill}"/>`;
  const groundOverlays = depth ? `${groundShape('url(#sheenBg)')}${groundShape('url(#vig)')}` : '';
  const tileViewBox = squircle ? ' viewBox="0 0 1024 1024"' : '';
  const tileSvg = `<svg width="${S}" height="${S}"${tileViewBox} xmlns="http://www.w3.org/2000/svg"><defs>${grad}${sheenBg}${vig}</defs>${groundShape('url(#bg)')}${groundOverlays}</svg>`;

  // White monitor with a glassy sheen; at larger sizes a diagonal specular
  // streak (top-left highlight) + an inner bottom shadow give the screen real
  // glass depth. The stand stays solid. Tinted modes (mark ≠ white) stay flat.
  const sheen = `<linearGradient id="sheen" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#e6eef6"/></linearGradient>`;
  const spec = `<linearGradient id="spec" x1="0" y1="0" x2="0.7" y2="1"><stop offset="0" stop-color="#ffffff" stop-opacity="0.7"/><stop offset="0.42" stop-color="#ffffff" stop-opacity="0"/></linearGradient>`;
  const inner = `<linearGradient id="inner" x1="0" y1="0" x2="0" y2="1"><stop offset="0.5" stop-color="#15263a" stop-opacity="0"/><stop offset="1" stop-color="#15263a" stop-opacity="0.15"/></linearGradient>`;
  const clip = `<clipPath id="scr"><rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" rx="${geo.sr}"/></clipPath>`;
  const screenFill = mark === WHITE ? 'url(#sheen)' : mark;
  const screenOverlays =
    mark === WHITE && depth
      ? `<g clip-path="url(#scr)"><rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" fill="url(#inner)"/><rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" fill="url(#spec)"/></g>`
      : '';
  const stand =
    geo.nw > 0
      ? `<rect x="${geo.nx}" y="${geo.ny}" width="${geo.nw}" height="${geo.nh}" fill="${mark}"/><rect x="${geo.bx}" y="${geo.by}" width="${geo.bw}" height="${geo.bh}" rx="${geo.br}" fill="${mark}"/>`
      : '';
  const monSvg = `<svg width="${S}" height="${S}" xmlns="http://www.w3.org/2000/svg"><defs>${sheen}${spec}${inner}${clip}</defs><rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" rx="${geo.sr}" fill="${screenFill}"/>${screenOverlays}${stand}</svg>`;

  // Spark knocked OUT of the monitor (dest-out) → the ground gradient shows through.
  const spk = Math.round(Math.min(geo.sw, geo.sh) * geo.sparkF);
  const sparkAlpha = await sharp(sparkPng).resize(spk, spk, { fit: 'contain' }).png().toBuffer();
  const spx = geo.sx + Math.round(geo.sw / 2 - spk / 2 - geo.sw * 0.06);
  const spy = geo.sy + Math.round(geo.sh / 2 - spk / 2 + geo.sh * 0.05);
  let mon = await sharp(Buffer.from(monSvg)).png().toBuffer();
  mon = await sharp(mon).composite([{ input: sparkAlpha, left: spx, top: spy, blend: 'dest-out' }]).png().toBuffer();

  let img = await sharp(Buffer.from(tileSvg)).png().toBuffer();
  if (S >= 96) {
    // Layered drop shadow under the monitor: a soft AMBIENT for lift + a tight
    // CONTACT for grounding (skipped at small sizes — it muddies).
    const shRect = (op) =>
      `<rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" rx="${geo.sr}" fill="#052a44" fill-opacity="${op}"/>${geo.nw > 0 ? `<rect x="${geo.bx}" y="${geo.by}" width="${geo.bw}" height="${geo.bh}" rx="${geo.br}" fill="#052a44" fill-opacity="${op}"/>` : ''}`;
    const ambient = await sharp(
      Buffer.from(`<svg width="${S}" height="${S}" xmlns="http://www.w3.org/2000/svg">${shRect(0.34)}</svg>`),
    )
      .blur(Math.max(1, S * 0.022))
      .png()
      .toBuffer();
    const contact = await sharp(
      Buffer.from(`<svg width="${S}" height="${S}" xmlns="http://www.w3.org/2000/svg">${shRect(0.26)}</svg>`),
    )
      .blur(Math.max(1, S * 0.007))
      .png()
      .toBuffer();
    img = await sharp(img)
      .composite([
        { input: ambient, left: 0, top: Math.round(S * 0.024) },
        { input: contact, left: 0, top: Math.round(S * 0.009) },
      ])
      .png()
      .toBuffer();
  }
  img = await sharp(img).composite([{ input: mon }]).png().toBuffer();
  return img;
}

/**
 * macOS icon following Apple's grid: the squircle is 824×824 centred in a 1024
 * canvas (100px transparent gutter on all sides, corner radius ≈185.4). Filling
 * the whole 1024 makes the dock icon read ~24% larger than other apps.
 */
async function macGridIcon(ground, mark) {
  const inner = await compose(824, ground, mark, { squircle: true }); // 824 squircle
  return sharp({ create: { width: 1024, height: 1024, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
    .composite([{ input: inner, left: 100, top: 100 }])
    .png()
    .toBuffer();
}

/** Monochrome tray/template glyph: single-colour monitor with the spark cut to transparent. */
async function trayMono(S, color) {
  const geo = geometry(S, 0, S <= 32);
  const base = await sharp(Buffer.from(monitorSvg(S, geo, color))).png().toBuffer();
  const spk = Math.round(Math.min(geo.sw, geo.sh) * geo.sparkF);
  const sprite = await sharp(sparkPng).resize(spk, spk, { fit: 'contain' }).png().toBuffer();
  const spx = geo.sx + Math.round(geo.sw / 2 - spk / 2 - geo.sw * 0.06);
  const spy = geo.sy + Math.round(geo.sh / 2 - spk / 2 + geo.sh * 0.05);
  // dest-out: the spark's alpha erases the screen → transparent hole.
  return sharp(base).composite([{ input: sprite, left: spx, top: spy, blend: 'dest-out' }]).png().toBuffer();
}

/** A self-contained SVG master (full-bleed) embedding the ground-tinted spark — for source/reference. */
async function masterSvg() {
  const S = 1024;
  const geo = geometry(S, 0);
  const spk = Math.round(Math.min(geo.sw, geo.sh) * 0.92);
  const blueSpark = await coloredSpark(spk, BLUE);
  const spx = geo.sx + Math.round(geo.sw / 2 - spk / 2 - geo.sw * 0.06);
  const spy = geo.sy + Math.round(geo.sh / 2 - spk / 2 + geo.sh * 0.05);
  const b64 = blueSpark.toString('base64');
  return `<svg viewBox="0 0 ${S} ${S}" width="${S}" height="${S}" xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">
  <!-- AHV Remote Console — monitor + spark (knocked out). Generated by scripts/build-icons.mjs. -->
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="${adjust(BLUE, 0.24)}"/><stop offset="0.5" stop-color="${adjust(BLUE, 0.02)}"/><stop offset="1" stop-color="${adjust(BLUE, -0.26)}"/></linearGradient>
    <linearGradient id="sheen" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#e6eef6"/></linearGradient>
  </defs>
  <rect width="${S}" height="${S}" fill="url(#bg)"/>
  <rect x="${geo.sx}" y="${geo.sy}" width="${geo.sw}" height="${geo.sh}" rx="${geo.sr}" fill="url(#sheen)"/>
  <rect x="${geo.nx}" y="${geo.ny}" width="${geo.nw}" height="${geo.nh}" fill="${WHITE}"/>
  <rect x="${geo.bx}" y="${geo.by}" width="${geo.bw}" height="${geo.bh}" rx="${geo.br}" fill="${WHITE}"/>
  <image x="${spx}" y="${spy}" width="${spk}" height="${spk}" xlink:href="data:image/png;base64,${b64}"/>
</svg>`;
}

// macOS Tahoe rendering modes (1024 PNGs). default/dark are the real two-tone
// icon; tinted/clear are flat tonal derivations (the OS applies tint/glass).
const MODE_COLOURS = {
  'icon-default': { ground: BLUE, mark: WHITE },
  'icon-dark': { ground: INK, mark: WHITE },
  'icon-tinted-light': { ground: '#DDE6EF', mark: INK },
  'icon-tinted-dark': { ground: '#2A3B4D', mark: WHITE },
  'icon-clear-light': { ground: '#9FBEDC', mark: WHITE },
  'icon-clear-dark': { ground: '#23425E', mark: WHITE },
};

async function main() {
  for (const d of [ASSETS, BUILD, MODES, PREVIEW, SOURCE]) mkdirSync(d, { recursive: true });

  console.log('Composing full-bleed PNGs (Windows / Linux / web)...');
  const png = {};
  for (const size of SIZES) {
    png[size] = await compose(size, BLUE, WHITE);
    writeFileSync(join(ASSETS, `icon-${size}.png`), png[size]);
    writeFileSync(join(PREVIEW, `icon-${size}.png`), png[size]);
    writeFileSync(join(BUILD, `${size}x${size}.png`), png[size]); // Linux hicolor
    console.log(`  icon-${size}.png`);
  }
  writeFileSync(join(BUILD, 'ahv-remote-console-512.png'), png[512]); // runtime BrowserWindow icon (win/linux)

  // macOS runtime dock icon (app.dock.setIcon, dev + packaged): squircle on the
  // 824/1024 Apple grid so it matches the size + shape of other dock icons.
  const macIcon = await macGridIcon(BLUE, WHITE);
  writeFileSync(join(BUILD, 'ahv-remote-console-dock.png'), macIcon);

  // Tray (monochrome): template = black-on-transparent (macOS recolours it).
  writeFileSync(join(BUILD, 'tray-iconTemplate.png'), await trayMono(16, '#000000'));
  writeFileSync(join(BUILD, 'tray-icon.png'), await trayMono(32, '#FFFFFF'));

  // .icns (macOS) — squircle on the 824/1024 Apple grid.
  console.log('Generating .icns (824/1024 squircle)...');
  const icns = png2icons.createICNS(macIcon, png2icons.BICUBIC2, 0);
  if (!icns) {
    console.error('  FAILED to generate .icns');
    process.exitCode = 1;
  } else {
    writeFileSync(join(BUILD, 'ahv-remote-console.icns'), icns);
    writeFileSync(join(ASSETS, 'icon.icns'), icns);
    console.log('  icon.icns');
  }

  // .ico (Windows) — FULL-BLEED.
  console.log('Generating .ico (full-bleed)...');
  const ico = png2icons.createICO(png[1024], png2icons.BICUBIC2, 0, true);
  if (!ico) {
    console.error('  FAILED to generate .ico');
    process.exitCode = 1;
  } else {
    writeFileSync(join(BUILD, 'ahv-remote-console.ico'), ico);
    writeFileSync(join(ASSETS, 'icon.ico'), ico);
    console.log('  icon.ico');
  }

  // Source SVG master (full-bleed, embedded spark) + a copy in build/.
  const svg = await masterSvg();
  writeFileSync(join(SOURCE, 'icon-master.svg'), svg);
  writeFileSync(join(BUILD, 'icon.svg'), svg);

  // macOS Tahoe rendering modes.
  console.log('Generating Tahoe rendering modes...');
  for (const [name, { ground, mark }] of Object.entries(MODE_COLOURS)) {
    writeFileSync(join(MODES, `${name}.png`), await compose(1024, ground, mark));
    console.log(`  ${name}.png`);
  }

  console.log('Done.');
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
