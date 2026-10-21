'use strict';

// Generate integrity.json mapping <platform>-<arch> -> sha256(node.napi.node)
// for every prebuilt binary, so the loader can pin-verify the unpacked .node
// before dlopen. Run AFTER prebuildify (see package.json `build:native`); the
// manifest is then packed inside the asar (it is not a .node), while the
// binaries it pins live in app.asar.unpacked. No-op-writes an empty manifest if
// there are no prebuilds yet.

const fs = require('node:fs');
const path = require('node:path');
const { sha256File } = require('../integrity');

const root = path.join(__dirname, '..');
const prebuildsDir = path.join(root, 'prebuilds');

const manifest = {};
if (fs.existsSync(prebuildsDir)) {
  for (const key of fs.readdirSync(prebuildsDir)) {
    const file = path.join(prebuildsDir, key, 'node.napi.node');
    if (fs.existsSync(file)) manifest[key] = sha256File(file);
  }
}

fs.writeFileSync(path.join(root, 'integrity.json'), JSON.stringify(manifest, null, 2) + '\n');
console.log('native-keyhook: wrote integrity.json', JSON.stringify(manifest));
