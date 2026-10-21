#!/usr/bin/env node
// Generate THIRD_PARTY_NOTICES.md from tooling output. NOT hand-edited.
//
//   Go runtime binary  -> go-licenses (csv for the manifest, save for texts)
//   In-guest agent      -> go-licenses, swept over GOOS {windows,linux,darwin}
//   Desktop app (prod) -> license-checker over apps/desktop production deps
//   Electron runtime   -> its own MIT license + a pointer to the Chromium/
//                         ffmpeg/etc. notices Electron ships in the app bundle
//
// RELE-01: the in-guest agent is NOT stdlib-only — it links golang.org/x/sys
// (unix on Linux, windows on Windows) plus OS-gated credential/D-Bus backends.
// Because those imports sit behind build constraints, a single-GOOS go-licenses
// pass silently dropped the Windows (wincred) and Linux (godbus/dbus) ones, so
// we sweep all three target GOOS and union the component set. cmd/isobuilder is
// still excluded: it is a build-time tool that is never distributed.
//
// Regenerate:  node scripts/gen-third-party-notices.mjs   (or: pnpm notices:generate)
//
// Requires: go, go-licenses (go install github.com/google/go-licenses@latest),
// and network access to npm for license-checker.

import { execSync } from 'node:child_process'
import {
  readFileSync, writeFileSync, existsSync, readdirSync, statSync, mkdtempSync, rmSync,
} from 'node:fs'
import { join, relative, dirname } from 'node:path'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
import { createHash } from 'node:crypto'

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
const run = (cmd, opts = {}) =>
  execSync(cmd, { encoding: 'utf8', maxBuffer: 128 * 1024 * 1024, ...opts })

const MODULE = 'github.com/MarcoCarvin/ahv-remote-console'
const RUNTIME_PKG = `${MODULE}/apps/runtime/cmd/ahv-remote-console-runtime`
const AGENT_PKG = `${MODULE}/apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent`

// Each shipped Go binary, with the group it appears under in the notices.
// RELE-01: the in-guest agent is NOT stdlib-only — it imports golang.org/x/sys
// (unix on Linux, windows on Windows) and pulls in OS-specific credential/D-Bus
// backends. It MUST be swept. (The firmware-write helper is now folded into the
// agent binary, so AGENT_PKG covers its deps too.)
const GO_BINARIES = [
  { pkg: RUNTIME_PKG, group: 'Go — runtime broker binary' },
  { pkg: AGENT_PKG, group: 'Go — in-guest agent binary' },
]

// RELE-01: a single host GOOS misses platform-gated imports. The Windows
// credential backend (danieljoos/wincred) and the Linux secret-service backend
// (godbus/dbus) are behind build constraints, so a darwin-only sweep silently
// dropped them. Sweep all three target GOOS and union the results.
const GOOS_SWEEP = ['windows', 'linux', 'darwin']

/** @type {{name:string,version:string,license:string,url:string,text:string,group:string}[]} */
const components = []

function goComponents() {
  const gopath = run('go env GOPATH').trim()
  const goLic = join(gopath, 'bin', 'go-licenses')
  if (!existsSync(goLic)) {
    throw new Error(`go-licenses not found at ${goLic}\nInstall: go install github.com/google/go-licenses@latest`)
  }

  // Module -> version (longest-prefix match against the package path in the
  // CSV). GOOS-independent, so compute it once over the whole module graph.
  const vers = {}
  for (const line of run("go list -m -f '{{.Path}} {{.Version}}' all", { cwd: repoRoot }).trim().split('\n')) {
    const sp = line.indexOf(' ')
    if (sp > 0) vers[line.slice(0, sp)] = line.slice(sp + 1)
  }
  const versionOf = (mod) => {
    let version = '', best = ''
    for (const p of Object.keys(vers)) {
      if ((mod === p || mod.startsWith(p + '/')) && p.length > best.length) { best = p; version = vers[p] }
    }
    return version
  }

  // (group -> Map(module -> component)) so a module that shows up under several
  // GOOS / binaries is recorded once per group, unioned across the sweep.
  const byGroup = new Map()

  for (const { pkg, group } of GO_BINARIES) {
    if (!byGroup.has(group)) byGroup.set(group, new Map())
    const groupComps = byGroup.get(group)

    for (const GOOS of GOOS_SWEEP) {
      // go-licenses honors GOOS/GOARCH from the environment, so each pass sees
      // that platform's build-constrained imports. amd64 is supported by every
      // target GOOS here, so we hold GOARCH fixed and vary only GOOS.
      const env = { ...process.env, GOOS, GOARCH: 'amd64', CGO_ENABLED: '0' }
      const execOpts = { cwd: repoRoot, env }

      let csv
      try {
        csv = run(`${goLic} csv ${pkg} --ignore ${MODULE}`, {
          ...execOpts, stdio: ['ignore', 'pipe', 'ignore'],
        }).trim().split('\n').filter(Boolean)
      } catch {
        // A binary may not build for a given GOOS (e.g. a windows-only helper);
        // skip that (binary, GOOS) pair rather than fail the whole generation.
        continue
      }

      // Dump LICENSE files for this pass so we can embed their texts.
      const saveDir = mkdtempSync(join(tmpdir(), 'gonotices-'))
      try {
        run(`${goLic} save ${pkg} --save_path=${saveDir} --ignore ${MODULE} --force`, {
          ...execOpts, stdio: ['ignore', 'ignore', 'ignore'],
        })
      } catch {
        // `save` can be flakier than `csv` (e.g. unrecognized license type);
        // proceed with whatever it managed to write.
      }
      const saved = [] // {mod, text}
      const walk = (dir) => {
        let entries
        try {
          entries = readdirSync(dir)
        } catch {
          // `go-licenses save --force` deletes save_path before writing; if that
          // save then failed (caught above) the dir is gone. Skip gracefully —
          // this pass contributes no license texts; another GOOS pass backfills.
          return
        }
        for (const e of entries) {
          const p = join(dir, e)
          if (statSync(p).isDirectory()) walk(p)
          else if (/^(LICENSE|LICENCE|COPYING)/i.test(e)) {
            saved.push({ mod: relative(saveDir, dirname(p)).split(/[\\/]/).join('/'), text: readFileSync(p, 'utf8') })
          }
        }
      }
      walk(saveDir)
      saved.sort((a, b) => b.mod.length - a.mod.length) // longest path first for prefix match

      for (const row of csv) {
        const [mod, url, license] = row.split(',')
        if (!mod || mod.startsWith(MODULE)) continue
        const hit = saved.find((s) => mod === s.mod || mod.startsWith(s.mod + '/'))
        const existing = groupComps.get(mod)
        // Union: keep the first sighting, but backfill a license text if a later
        // GOOS pass managed to dump one this pass didn't have.
        if (!existing) {
          groupComps.set(mod, {
            name: mod, version: versionOf(mod), license, url, text: hit?.text || '', group,
          })
        } else if (!existing.text && hit?.text) {
          existing.text = hit.text
        }
      }
      rmSync(saveDir, { recursive: true, force: true })
    }
  }

  for (const groupComps of byGroup.values()) {
    for (const c of groupComps.values()) components.push(c)
  }
}

function nodeComponents() {
  const raw = run('npx --yes license-checker --production --json', {
    cwd: join(repoRoot, 'apps', 'desktop'),
    stdio: ['ignore', 'pipe', 'ignore'],
  })
  const json = JSON.parse(raw)
  for (const [key, info] of Object.entries(json)) {
    const license = info.licenses || 'UNKNOWN'
    if (license === 'UNLICENSED' || key.startsWith('@ahv-remote-console/')) continue // first-party
    const at = key.lastIndexOf('@')
    const name = key.slice(0, at)
    const version = key.slice(at + 1)
    let text = ''
    if (info.licenseFile && existsSync(info.licenseFile)) text = readFileSync(info.licenseFile, 'utf8')
    components.push({ name, version, license, url: info.repository || '', text, group: 'JavaScript — desktop app bundle' })
  }
}

function electronComponent() {
  const candidates = [
    join(repoRoot, 'apps', 'desktop', 'node_modules', 'electron'),
    join(repoRoot, 'node_modules', 'electron'),
  ]
  const dir = candidates.find((d) => existsSync(join(d, 'package.json')))
  if (!dir) return
  const version = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')).version || ''
  const licFile = ['LICENSE', 'LICENSE.md', 'license'].map((f) => join(dir, f)).find(existsSync)
  const text = licFile ? readFileSync(licFile, 'utf8') : ''
  components.push({
    name: 'electron',
    version,
    license: 'MIT',
    url: 'https://github.com/electron/electron',
    text:
      (text ? text + '\n\n' : '') +
      'Electron embeds Chromium, V8, Node.js and FFmpeg, which carry their own ' +
      'licenses (Chromium: BSD-3-Clause and others; FFmpeg: LGPL-2.1-or-later in the ' +
      'shipped build). The complete per-component texts are distributed inside the ' +
      'application bundle as LICENSE and LICENSES.chromium.html within the Electron ' +
      'Framework.',
    group: 'Electron runtime (bundled with the desktop app)',
  })
}

// ---- assemble ----
goComponents()
nodeComponents()
electronComponent()

components.sort((a, b) => a.group.localeCompare(b.group) || a.name.localeCompare(b.name))

// Dedupe license texts so identical texts (e.g. Apache-2.0) appear once.
const textGroups = new Map() // hash -> { text, components: string[] }
const hashOf = (t) => createHash('sha256').update(t.replace(/\s+/g, ' ').trim()).digest('hex').slice(0, 12)
for (const c of components) {
  if (!c.text) continue
  const h = hashOf(c.text)
  if (!textGroups.has(h)) textGroups.set(h, { text: c.text, components: [] })
  textGroups.get(h).components.push(`${c.name}@${c.version || '—'}`)
}

const groups = [...new Set(components.map((c) => c.group))]
let md = `# Third-Party Notices

> AUTO-GENERATED by \`scripts/gen-third-party-notices.mjs\` — do not edit by hand.
> Regenerate with \`pnpm notices:generate\`.

AHV Remote Console incorporates the third-party open-source
components listed below, each governed by its own license (reproduced in the
"License texts" section). This satisfies the attribution obligation referenced
in clause 3 of [\`LICENSE\`](./LICENSE).

Components: **${components.length}**. The in-guest agent links
\`golang.org/x/sys\` (the \`unix\` package on
Linux, the \`windows\` package on Windows) plus the platform credential/D-Bus
backends those pull in; its third-party components are enumerated above,
swept across all target operating systems. \`cmd/isobuilder\` is a build-time
tool and is not distributed — it contributes no third-party components.

`

for (const g of groups) {
  const rows = components.filter((c) => c.group === g)
  md += `## ${g}\n\n| Component | Version | License | Source |\n|---|---|---|---|\n`
  for (const c of rows) {
    const src = c.url ? c.url.replace(/^git\+/, '').replace(/\.git$/, '') : '—'
    md += `| ${c.name} | ${c.version || '—'} | ${c.license} | ${src} |\n`
  }
  md += '\n'
}

md += `## License texts\n\n`
for (const { text, components: cs } of textGroups.values()) {
  md += `<details>\n<summary>Applies to: ${cs.join(', ')}</summary>\n\n\`\`\`\n${text.trim()}\n\`\`\`\n\n</details>\n\n`
}

writeFileSync(join(repoRoot, 'THIRD_PARTY_NOTICES.md'), md)
console.log(`Wrote THIRD_PARTY_NOTICES.md — ${components.length} components, ${textGroups.size} distinct license texts.`)
