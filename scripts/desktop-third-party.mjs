#!/usr/bin/env node
// The licence bundle the GPUI desktop release ships beside dilla-desktop (GPUI release plan, Task 2):
//   LICENSE                  Dilla's own licence (AGPL-3.0-or-later, the repository's LICENSE)
//   OFL-JetBrainsMono.txt    the embedded fonts' licences (dilla-desktop-ui/assets/fonts)
//   OFL-DMSerifDisplay.txt
//   libwebrtc-LICENSE.md     LiveKit's prebuilt libwebrtc, linked statically (its archive's LICENSE.md)
//   THIRD-PARTY.md           every crate linked into dilla-desktop on x86_64 Linux: name, version, licence, repository
//
// THIRD-PARTY.md comes from `cargo metadata --locked --filter-platform <target>`: the crates reachable from
// dilla-desktop through normal dependencies (build scripts and dev-dependencies ship nothing). A crate with neither a
// `license` nor a `license_file` fails the run, so the bundle never silently misses one.
//
// Usage: desktop-third-party.mjs <out-dir> --webrtc-license FILE [--target TRIPLE] [--metadata FILE]
import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
export const ROOT_CRATE = 'dilla-desktop';
export const STATIC_FILES = {
  LICENSE: 'LICENSE',
  'OFL-JetBrainsMono.txt': 'clients/desktop/dilla-desktop-ui/assets/fonts/OFL-JetBrainsMono.txt',
  'OFL-DMSerifDisplay.txt': 'clients/desktop/dilla-desktop-ui/assets/fonts/OFL-DMSerifDisplay.txt',
};

/** The packages reachable from `rootName` through normal dependencies, sorted by name then version. */
export function shippedPackages(metadata, rootName = ROOT_CRATE) {
  const byId = new Map(metadata.packages.map((p) => [p.id, p]));
  const nodes = new Map((metadata.resolve?.nodes ?? []).map((n) => [n.id, n]));
  const roots = metadata.packages.filter((p) => p.name === rootName && metadata.workspace_members.includes(p.id));
  if (roots.length !== 1) throw new Error(`desktop-third-party: ${rootName} is not exactly one workspace member`);
  const seen = new Set();
  const queue = [roots[0].id];
  while (queue.length > 0) {
    const id = queue.shift();
    if (seen.has(id)) continue;
    seen.add(id);
    const node = nodes.get(id);
    if (!node) throw new Error(`desktop-third-party: ${id} is not in the resolve graph`);
    for (const dep of node.deps ?? []) {
      if ((dep.dep_kinds ?? []).some((k) => k.kind === null)) queue.push(dep.pkg);
    }
  }
  seen.delete(roots[0].id);
  return [...seen].map((id) => byId.get(id)).sort((a, b) => a.name.localeCompare(b.name) || a.version.localeCompare(b.version));
}

const cell = (s) => String(s).replace(/\|/g, '\\|').replace(/\s+/g, ' ').trim();

/** THIRD-PARTY.md's text. Workspace crates are Dilla's own (LICENSE) and are not listed. */
export function thirdPartyMarkdown(metadata, rootName = ROOT_CRATE) {
  const members = new Set(metadata.workspace_members);
  const rows = [];
  for (const p of shippedPackages(metadata, rootName)) {
    if (members.has(p.id)) continue;
    const licence = p.license ?? (p.license_file ? `see ${p.license_file} in the crate` : null);
    if (!licence) throw new Error(`desktop-third-party: ${p.name} ${p.version} declares no licence`);
    rows.push(`| ${cell(p.name)} | ${cell(p.version)} | ${cell(licence)} | ${cell(p.repository ?? p.homepage ?? `https://crates.io/crates/${p.name}`)} |`);
  }
  return [
    '# Third-party software in Dilla',
    '',
    'The Dilla desktop app (dilla-desktop) links these Rust crates. Each is used under the licence named here; the',
    'full texts are in each crate\'s source at the repository given. LiveKit\'s libwebrtc is linked statically',
    '(libwebrtc-LICENSE.md), and the embedded fonts are under the SIL Open Font License (OFL-*.txt).',
    '',
    '| Crate | Version | Licence | Repository |',
    '|---|---|---|---|',
    ...rows,
    '',
  ].join('\n');
}

export function writeBundle(outDir, { metadata, webrtcLicense, root = ROOT }) {
  mkdirSync(outDir, { recursive: true });
  for (const [name, rel] of Object.entries(STATIC_FILES)) copyFileSync(join(root, rel), join(outDir, name));
  copyFileSync(webrtcLicense, join(outDir, 'libwebrtc-LICENSE.md'));
  writeFileSync(join(outDir, 'THIRD-PARTY.md'), thirdPartyMarkdown(metadata));
}

// Run directly, on every OS: a Windows path (D:\a\...) or one with a space never equals `file://` + argv[1].
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const args = process.argv.slice(2);
  const flag = (name) => { const i = args.indexOf(name); if (i < 0) return undefined; const v = args[i + 1]; args.splice(i, 2); return v; };
  const webrtcLicense = flag('--webrtc-license');
  const target = flag('--target') ?? 'x86_64-unknown-linux-gnu';
  const metadataFile = flag('--metadata');
  if (args.length !== 1 || !webrtcLicense) {
    console.error('usage: desktop-third-party.mjs <out-dir> --webrtc-license FILE [--target TRIPLE] [--metadata FILE]');
    process.exit(1);
  }
  const metadata = JSON.parse(metadataFile
    ? readFileSync(metadataFile, 'utf8')
    : execFileSync('cargo', ['metadata', '--format-version', '1', '--locked', '--filter-platform', target], { cwd: ROOT, maxBuffer: 256 << 20, encoding: 'utf8' }));
  writeBundle(args[0], { metadata, webrtcLicense });
}
