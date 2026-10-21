#!/usr/bin/env node
/**
 * build-icon-sprite.mjs — combines individual SVG icons into a single
 * <svg> sprite with <symbol> wrappers for use with <use href="#name"/>.
 */
import { readdirSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { join, basename, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ICONS_DIR = join(__dirname, '..', 'assets', 'icons');
const OUT_DIR = join(__dirname, '..', 'apps', 'desktop', 'public', 'icons');

mkdirSync(OUT_DIR, { recursive: true });

const files = readdirSync(ICONS_DIR).filter((f) => f.endsWith('.svg')).sort();
const symbols = [];

for (const file of files) {
  const id = basename(file, '.svg');
  const raw = readFileSync(join(ICONS_DIR, file), 'utf8');

  const viewBoxMatch = raw.match(/viewBox="([^"]+)"/);
  const viewBox = viewBoxMatch ? viewBoxMatch[1] : '0 0 24 24';

  const inner = raw
    .replace(/<svg[^>]*>/, '')
    .replace(/<\/svg>/, '')
    .trim();

  symbols.push(`  <symbol id="${id}" viewBox="${viewBox}">\n    ${inner}\n  </symbol>`);
}

const sprite = `<svg xmlns="http://www.w3.org/2000/svg" style="display:none">\n${symbols.join('\n')}\n</svg>\n`;
writeFileSync(join(OUT_DIR, 'sprite.svg'), sprite);
console.log(`Sprite: ${files.length} icons → ${OUT_DIR}/sprite.svg`);
