import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
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

test('the repository passes', { todo: 'Task 3' }, () => {
  assert.deepEqual(checkDocsShots(repoRoot), []);
});
