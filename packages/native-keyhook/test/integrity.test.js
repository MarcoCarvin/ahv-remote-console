'use strict';

const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { sha256File, resolveVerifiedBinary } = require('../integrity');

function tmpPkg() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'keyhook-'));
}
function writePrebuild(dir, key, bytes) {
  const pd = path.join(dir, 'prebuilds', key);
  fs.mkdirSync(pd, { recursive: true });
  const file = path.join(pd, 'node.napi.node');
  fs.writeFileSync(file, bytes);
  return file;
}
function writeManifest(dir, obj) {
  fs.writeFileSync(path.join(dir, 'integrity.json'), JSON.stringify(obj));
}

test('shipped: loads the prebuild when the pinned hash matches', () => {
  const dir = tmpPkg();
  const f = writePrebuild(dir, 'linux-x64', Buffer.from('genuine-binary'));
  writeManifest(dir, { 'linux-x64': sha256File(f) });
  assert.strictEqual(resolveVerifiedBinary(dir, 'linux', 'x64'), f);
});

test('shipped: refuses a tampered prebuild (hash mismatch -> null)', () => {
  const dir = tmpPkg();
  writePrebuild(dir, 'linux-x64', Buffer.from('tampered-binary'));
  writeManifest(dir, { 'linux-x64': 'a'.repeat(64) }); // wrong, well-formed hash
  assert.strictEqual(resolveVerifiedBinary(dir, 'linux', 'x64'), null);
});

test('shipped: refuses when no pin exists for this platform-arch', () => {
  const dir = tmpPkg();
  writePrebuild(dir, 'win32-x64', Buffer.from('x'));
  writeManifest(dir, { 'linux-x64': 'a'.repeat(64) });
  assert.strictEqual(resolveVerifiedBinary(dir, 'win32', 'x64'), null);
});

test('shipped: never falls back to an unpinned node-gyp binary an attacker planted', () => {
  const dir = tmpPkg();
  fs.mkdirSync(path.join(dir, 'build', 'Release'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'build', 'Release', 'keyhook.node'), Buffer.from('planted'));
  writeManifest(dir, {}); // manifest present (shipped) but empty
  assert.strictEqual(resolveVerifiedBinary(dir, 'linux', 'x64'), null);
});

test('shipped: refuses when the pinned prebuild file is missing', () => {
  const dir = tmpPkg();
  writeManifest(dir, { 'linux-x64': 'a'.repeat(64) });
  assert.strictEqual(resolveVerifiedBinary(dir, 'linux', 'x64'), null);
});

test('dev: no manifest -> loads the node-gyp build unverified', () => {
  const dir = tmpPkg();
  fs.mkdirSync(path.join(dir, 'build', 'Release'), { recursive: true });
  const f = path.join(dir, 'build', 'Release', 'keyhook.node');
  fs.writeFileSync(f, Buffer.from('dev-build'));
  assert.strictEqual(resolveVerifiedBinary(dir, 'darwin', 'arm64'), f);
});

test('dev: no manifest -> prefers a prebuild over a node-gyp build', () => {
  const dir = tmpPkg();
  const pre = writePrebuild(dir, 'darwin-arm64', Buffer.from('pre'));
  fs.mkdirSync(path.join(dir, 'build', 'Release'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'build', 'Release', 'keyhook.node'), Buffer.from('gyp'));
  assert.strictEqual(resolveVerifiedBinary(dir, 'darwin', 'arm64'), pre);
});

test('nothing present -> null', () => {
  assert.strictEqual(resolveVerifiedBinary(tmpPkg(), 'linux', 'x64'), null);
});
