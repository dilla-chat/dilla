import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { checkDocsShots } from './check-docs-shots.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

function tree({ docs = {}, images = [], manifest = { shots: {}, manual: {} } }) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'docs-shots-'));
  mkdirSync(path.join(root, 'docs/user/screenshots'), { recursive: true });
  for (const [f, text] of Object.entries(docs)) writeFileSync(path.join(root, f), text);
  for (const f of images) writeFileSync(path.join(root, 'docs/user/screenshots', f), 'png');
  writeFileSync(path.join(root, 'docs/user/screenshots/manifest.json'), JSON.stringify(manifest));
  return root;
}

test('a doc linking a missing image is reported', () => {
  const root = tree({ docs: { 'README.md': '![x](docs/user/screenshots/gone.png)' } });
  const p = checkDocsShots(root);
  assert.ok(p.some((l) => l.includes('missing') && l.includes('docs/user/screenshots/gone.png')), p.join('\n'));
});

test('an image no doc links is reported', () => {
  const root = tree({ images: ['lonely.png'], manifest: { shots: {}, manual: { 'lonely.png': 'by hand' } } });
  const p = checkDocsShots(root);
  assert.ok(p.some((l) => l.includes('unlinked') && l.includes('lonely.png')), p.join('\n'));
});

test('an image with neither a manifest shot nor a manual entry is reported', () => {
  const root = tree({ docs: { 'docs/user/a.md': '<img src="screenshots/a.png">' }, images: ['a.png'] });
  const p = checkDocsShots(root);
  assert.ok(p.some((l) => l.includes('no manifest entry') && l.includes('a.png')), p.join('\n'));
});

test('a manifest shot whose file is absent is reported', () => {
  const root = tree({ manifest: { shots: { 'b.png': { test: 'x::y', frame: 'f.png' } }, manual: {} } });
  const p = checkDocsShots(root);
  assert.ok(p.some((l) => l.includes('not shot') && l.includes('b.png')), p.join('\n'));
});

test('a linked, made image passes, and a case mismatch is reported', () => {
  const manifest = { shots: { 'a.png': { test: 'x::y', frame: 'f.png' } }, manual: {} };
  const ok = tree({ docs: { 'docs/user/a.md': '![a](screenshots/a.png)' }, images: ['a.png'], manifest });
  assert.deepEqual(checkDocsShots(ok), []);
  const bad = tree({ docs: { 'docs/user/a.md': '![a](screenshots/A.png)' }, images: ['a.png'], manifest });
  assert.ok(checkDocsShots(bad).some((l) => l.includes('missing')));
});

test('a sidecar file in the screenshots directory is reported', () => {
  const root = tree({});
  writeFileSync(path.join(root, 'docs/user/screenshots/a.layout.json'), '{}');
  assert.ok(checkDocsShots(root).some((l) => l.includes('a.layout.json') && l.includes('not a .png')));
});

test('external links are skipped and a root-absolute link resolves against the repo root', () => {
  const manifest = { shots: { 'a.png': { test: 'x::y', frame: 'f.png' } }, manual: {} };
  const root = tree({
    docs: {
      'docs/user/a.md': [
        '![a](/docs/user/screenshots/a.png)',
        '![e](https://example.com/screenshots/x.png)',
        '<img src="//cdn.example.com/screenshots/y.png">',
      ].join('\n'),
    },
    images: ['a.png'],
    manifest,
  });
  assert.deepEqual(checkDocsShots(root), []);
  const bad = tree({ docs: { 'README.md': '![a](/screenshots/nope.png)' } });
  assert.ok(checkDocsShots(bad).some((l) => l.includes('missing')));
});

test('links inside fenced code blocks are ignored', () => {
  const root = tree({ docs: { 'README.md': 'text\n\n```md\n![x](docs/user/screenshots/gone.png)\n```\n\n~~~\n![y](screenshots/gone2.png)\n~~~\n' } });
  assert.deepEqual(checkDocsShots(root), []);
  const after = tree({ docs: { 'README.md': '```\ncode\n```\n![x](docs/user/screenshots/gone.png)\n' } });
  assert.ok(checkDocsShots(after).some((l) => l.includes('missing')));
});

test('the shooter removes target/debug/incremental when it fails', () => {
  const root = mkdtempSync(path.join(os.tmpdir(), 'docs-shots-run-'));
  mkdirSync(path.join(root, 'docs/user/screenshots'), { recursive: true });
  mkdirSync(path.join(root, 'target/debug/incremental'), { recursive: true });
  mkdirSync(path.join(root, 'bin'));
  writeFileSync(path.join(root, 'bin/cargo'), '#!/bin/sh\nexit 1\n');
  chmodSync(path.join(root, 'bin/cargo'), 0o755);
  writeFileSync(
    path.join(root, 'docs/user/screenshots/manifest.json'),
    JSON.stringify({ shots: { 'a.png': { test: 'f::t', frame: 'a-gpui-2x.png' } }, manual: {} }),
  );
  const run = spawnSync('node', [path.join(repoRoot, 'scripts/docs-shots-desktop.mjs')], {
    env: { ...process.env, DOCS_SHOTS_ROOT: root, PATH: `${path.join(root, 'bin')}:${process.env.PATH}` },
    encoding: 'utf8',
  });
  assert.notEqual(run.status, 0);
  assert.match(run.stderr, /cargo test --test f failed/);
  assert.equal(existsSync(path.join(root, 'target/debug/incremental')), false);
});

test('the repository passes', () => {
  assert.deepEqual(checkDocsShots(repoRoot), []);
});
