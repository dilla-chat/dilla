#!/usr/bin/env node
// Re-shoots the desktop docs screenshots from the parity tests named in docs/user/screenshots/manifest.json.
//   node scripts/docs-shots-desktop.mjs [<name>.png ...]   (no names = all)
//   node scripts/docs-shots-desktop.mjs --list
// DOCS_SHOTS_ROOT overrides the repository root (the tests use it).
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, existsSync, readFileSync, rmSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = process.env.DOCS_SHOTS_ROOT ?? path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const shotsDir = path.join(root, 'docs/user/screenshots');
const framesDir = path.join(root, 'target/screen-parity');

function fail(message) {
  throw new Error(message);
}

function main() {
  const manifest = JSON.parse(readFileSync(path.join(shotsDir, 'manifest.json'), 'utf8'));
  const shots = manifest.shots ?? {};

  const args = process.argv.slice(2);
  if (args.includes('--list')) {
    for (const name of Object.keys(shots)) console.log(name);
    return;
  }

  const names = args.length ? args : Object.keys(shots);
  for (const n of names) {
    if (!(n in shots)) fail(`${n}: not in the manifest's shots`);
  }

  // One cargo invocation per test file, so the build is shared.
  const byFile = new Map();
  for (const name of names) {
    const sep = shots[name].test.indexOf('::');
    if (sep < 0) fail(`${name}: test "${shots[name].test}" is not <file>::<fn>`);
    const file = shots[name].test.slice(0, sep);
    const fn = shots[name].test.slice(sep + 2);
    if (!byFile.has(file)) byFile.set(file, []);
    byFile.get(file).push({ name, fn });
    rmSync(path.join(framesDir, shots[name].frame), { force: true });
  }

  const env = {
    ...process.env,
    CARGO_TARGET_DIR: path.join(root, 'target'),
    CC: 'clang',
    CXX: 'clang++',
    CARGO_BUILD_JOBS: '4',
  };

  try {
    for (const [file, entries] of byFile) {
      const fns = [...new Set(entries.map((e) => e.fn))];
      const cargoArgs = ['test', '-p', 'dilla-desktop', '--features', 'automation', '--test', file, '--', '--exact', ...fns, '--include-ignored'];
      const r = spawnSync('cargo', cargoArgs, { cwd: root, env, stdio: 'inherit' });
      if (r.status !== 0) fail(`${entries.map((e) => e.name).join(', ')}: cargo test --test ${file} failed`);
    }

    for (const name of names) {
      const { frame, crop } = shots[name];
      const src = path.join(framesDir, frame);
      if (!frame.endsWith('.png')) fail(`${name}: frame ${frame} is not a .png`);
      if (!existsSync(src)) fail(`${name}: the test passed but wrote no frame ${frame}`);
      const dest = path.join(shotsDir, name);
      if (!crop) {
        copyFileSync(src, dest);
        continue;
      }
      const [x, y, w, h] = crop;
      const size = execFileSync('magick', ['identify', '-format', '%w %h', src], { encoding: 'utf8' }).trim().split(' ').map(Number);
      if (![x, y, w, h].every(Number.isInteger) || x < 0 || y < 0 || w <= 0 || h <= 0 || x + w > size[0] || y + h > size[1]) {
        fail(`${name}: crop [${crop}] is outside the ${size[0]}x${size[1]} frame ${frame}`);
      }
      execFileSync('magick', [src, '-crop', `${w}x${h}+${x}+${y}`, '+repage', dest]);
    }
  } finally {
    rmSync(path.join(root, 'target/debug/incremental'), { recursive: true, force: true });
  }
  console.log(`shot ${names.length} screenshot(s)`);
}

try {
  main();
} catch (e) {
  console.error(e.message);
  process.exitCode = 1;
}
